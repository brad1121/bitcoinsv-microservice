package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bsvmspb "github.com/brad1121/bsvms/gen/bsvms/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type jwtClaims struct {
	Subject  string `json:"sub"`
	TenantID string `json:"tenant_id"`
	WalletID string `json:"wallet_id"`
	Type     string `json:"typ"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	JTI      string `json:"jti"`
}

type authSubject struct {
	TenantID string
	WalletID string
	Type     string
}

func (s *Service) AuthEnabled() bool {
	return s.opts.AuthEnabled
}

func (s *Service) UnaryAuthInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := s.authorize(ctx, info.FullMethod, req); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (s *Service) StreamAuthInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// The tenant check needs the request message, and a server-streaming
		// RPC only produces it when the generated handler calls RecvMsg. Wrap
		// the stream so the check runs there, before the handler body.
		return handler(srv, &authServerStream{ServerStream: stream, svc: s, method: info.FullMethod})
	}
}

// authServerStream authorizes the first message a stream receives.
type authServerStream struct {
	grpc.ServerStream
	svc     *Service
	method  string
	checked bool
}

func (a *authServerStream) RecvMsg(m any) error {
	if err := a.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if a.checked {
		return nil
	}
	a.checked = true
	return a.svc.authorize(a.ServerStream.Context(), a.method, m)
}

func (s *Service) RefreshToken(ctx context.Context, req *bsvmspb.RefreshTokenRequest) (*bsvmspb.AuthTokens, error) {
	if !s.opts.AuthEnabled {
		return nil, status.Error(codes.FailedPrecondition, "auth disabled")
	}
	sub, err := s.validateJWT(req.GetRefreshToken(), "refresh")
	if err != nil {
		return nil, err
	}
	return s.issueTokens(sub.TenantID, sub.WalletID)
}

func (s *Service) authorize(ctx context.Context, method string, req any) error {
	if !s.opts.AuthEnabled {
		return nil
	}
	if isBootstrapMethod(method) && s.bootstrapAllowed(req) {
		return nil
	}
	if strings.HasSuffix(method, "/RefreshToken") {
		return nil
	}
	sub, err := s.authSubjectFromContext(ctx)
	if err != nil {
		return err
	}
	if sub.Type != "access" {
		return status.Error(codes.Unauthenticated, "access token required")
	}
	// The closed-by-default rule governs this service's own methods. Anything
	// else registered on the same server — server reflection, health — is past
	// the token check above, which is all it ever had.
	if !isBSVMSMethod(method) {
		return nil
	}
	tenantID, walletID, scoped := tenantWalletFromRequest(req)
	if !scoped {
		if nodeScopedMethods[shortMethod(method)] {
			return nil
		}
		// Either an unknown method, or one whose request should carry a
		// tenant_id and does not. Refuse rather than wave it through.
		return status.Error(codes.PermissionDenied, "method is not tenant-scoped")
	}
	// An empty tenant_id is not a wildcard. The streaming filters treat it as
	// "match every tenant", so letting it past here would hand one tenant's
	// token every other tenant's events.
	if tenantID == "" {
		return status.Error(codes.PermissionDenied, "tenant_id required")
	}
	if tenantID != sub.TenantID {
		return status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	if walletID != "" && walletID != sub.WalletID {
		return status.Error(codes.PermissionDenied, "wallet mismatch")
	}
	return nil
}

func (s *Service) bootstrapAllowed(req any) bool {
	tenantID, walletID, scoped := tenantWalletFromRequest(req)
	if !scoped || tenantID == "" || walletID == "" {
		return false
	}
	if err := validateTenantWallet(tenantID, walletID); err != nil {
		return false
	}
	s.mu.Lock()
	_, exists := s.wallets[walletKey(tenantID, walletID)]
	s.mu.Unlock()
	return !exists
}

func isBootstrapMethod(method string) bool {
	return strings.HasSuffix(method, "/CreateWallet") || strings.HasSuffix(method, "/RestoreWallet")
}

func (s *Service) authSubjectFromContext(ctx context.Context) (*authSubject, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	var token string
	for _, value := range md.Get("authorization") {
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			token = strings.TrimSpace(value[7:])
			break
		}
	}
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	return s.validateJWT(token, "access")
}

func (s *Service) issueTokens(tenantID, walletID string) (*bsvmspb.AuthTokens, error) {
	now := time.Now().UTC()
	accessExp := now.Add(s.opts.AccessTTL)
	refreshExp := now.Add(s.opts.RefreshTTL)
	access, err := s.signJWT(jwtClaims{
		Subject:  tenantID + ":" + walletID,
		TenantID: tenantID,
		WalletID: walletID,
		Type:     "access",
		IssuedAt: now.Unix(),
		Expires:  accessExp.Unix(),
		JTI:      randomID(),
	})
	if err != nil {
		return nil, err
	}
	refresh, err := s.signJWT(jwtClaims{
		Subject:  tenantID + ":" + walletID,
		TenantID: tenantID,
		WalletID: walletID,
		Type:     "refresh",
		IssuedAt: now.Unix(),
		Expires:  refreshExp.Unix(),
		JTI:      randomID(),
	})
	if err != nil {
		return nil, err
	}
	return &bsvmspb.AuthTokens{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresAt:    accessExp.Format(time.RFC3339Nano),
		TenantId:     tenantID,
		WalletId:     walletID,
	}, nil
}

func (s *Service) signJWT(claims jwtClaims) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	head, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.opts.JWTSecret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) validateJWT(token, typ string) (*authSubject, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	unsigned := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token signature")
	}
	mac := hmac.New(sha256.New, s.opts.JWTSecret)
	mac.Write([]byte(unsigned))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, status.Error(codes.Unauthenticated, "invalid token signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token body")
	}
	var claims jwtClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token claims")
	}
	if claims.Type != typ {
		return nil, status.Error(codes.Unauthenticated, "wrong token type")
	}
	if time.Now().Unix() >= claims.Expires {
		return nil, status.Error(codes.Unauthenticated, "token expired")
	}
	if claims.TenantID == "" || claims.WalletID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing token subject")
	}
	return &authSubject{TenantID: claims.TenantID, WalletID: claims.WalletID, Type: claims.Type}, nil
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// nodeScopedMethods are the RPCs whose request carries no tenant_id because
// they act on the node or on pure data rather than on a tenant's wallet.
// Anything not listed here must carry a tenant_id, and is refused if it does
// not — so a wallet-scoped RPC added later is closed by default instead of
// silently unchecked.
var nodeScopedMethods = map[string]bool{
	"Status":                         true,
	"RefreshToken":                   true,
	"ConnectPeer":                    true,
	"BroadcastCustomSpend":           true,
	"P2PKHOutput":                    true,
	"OpReturnOutput":                 true,
	"AnyoneCanSpendOutput":           true,
	"ParseTransaction":               true,
	"BroadcastRaw":                   true,
	"ExecuteScript":                  true,
	"DecodeOutputAddress":            true,
	"PendingTransactions":            true,
	"RebroadcastPendingTransactions": true,
	"StreamTransactions":             true,
	"StreamBlocks":                   true,
	"StreamP2PTraffic":               true,
	"StreamRejects":                  true,
	"VerifyTxSeen":                   true,
	"WaitForTxRelay":                 true,
}

// bsvmsServicePrefix is "/bsvms.v1.BSVMS/", taken from the descriptor so it
// cannot drift from the proto package.
var bsvmsServicePrefix = "/" + string(bsvmspb.File_proto_bsvms_v1_bsvms_proto.Services().Get(0).FullName()) + "/"

func isBSVMSMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, bsvmsServicePrefix)
}

// shortMethod turns "/bsvms.v1.BSVMS/GetWallet" into "GetWallet".
func shortMethod(fullMethod string) string {
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		return fullMethod[i+1:]
	}
	return fullMethod
}

// tenantWalletFromRequest reads the tenant_id/wallet_id a request carries.
// scoped reports whether the message declares a tenant_id at all; reading the
// descriptor rather than naming each request type means a new wallet-scoped
// RPC is covered the moment its proto has the field.
func tenantWalletFromRequest(req any) (tenantID, walletID string, scoped bool) {
	msg, ok := req.(proto.Message)
	if !ok || msg == nil {
		return "", "", false
	}
	m := msg.ProtoReflect()
	if !m.IsValid() {
		return "", "", false
	}
	fields := m.Descriptor().Fields()
	tenantField := fields.ByName("tenant_id")
	if tenantField == nil || tenantField.Kind() != protoreflect.StringKind {
		return "", "", false
	}
	tenantID = m.Get(tenantField).String()
	if walletField := fields.ByName("wallet_id"); walletField != nil && walletField.Kind() == protoreflect.StringKind {
		walletID = m.Get(walletField).String()
	}
	return tenantID, walletID, true
}

func authContext(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", fmt.Sprintf("Bearer %s", token)))
}
