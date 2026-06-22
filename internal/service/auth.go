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
		if err := s.authorize(stream.Context(), info.FullMethod, nil); err != nil {
			return err
		}
		return handler(srv, stream)
	}
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
	tenantID, walletID := tenantWalletFromRequest(req)
	if tenantID == "" && walletID == "" {
		return nil
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
	tenantID, walletID := tenantWalletFromRequest(req)
	if tenantID == "" || walletID == "" {
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

func tenantWalletFromRequest(req any) (tenantID, walletID string) {
	switch r := req.(type) {
	case *bsvmspb.ListWalletsRequest:
		return r.GetTenantId(), ""
	case *bsvmspb.CreateWalletRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.RestoreWalletRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.GetWalletRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.NewAddressRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.BatchNewAddressesRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.DeriveAtRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.PubKeyAtRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.SignHashAtRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.NextIndexRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.BalanceRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.ListUTXOsRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.ImportUTXORequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.ProcessRawTxRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.ClearUTXOsRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.ReloadFromStoreRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.WipeWalletRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.PruneUnknownUTXOsRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.IgnoreOutpointRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.IsOutpointIgnoredRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.UntrackUTXORequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.CanCoverRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.SendRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.SendAllRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.SpendToOutputsRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.StreamPaymentsRequest:
		return r.GetTenantId(), r.GetWalletId()
	case *bsvmspb.StreamWalletTransactionsRequest:
		return r.GetTenantId(), r.GetWalletId()
	default:
		return "", ""
	}
}

func authContext(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", fmt.Sprintf("Bearer %s", token)))
}
