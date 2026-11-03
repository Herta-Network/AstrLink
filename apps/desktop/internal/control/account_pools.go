package control

import (
	"context"
	"encoding/json"
	"net/http"
)

const accountPoolsPath = "/control/v1/account-pools"

func (s Services) ListAccountPools(ctx context.Context) (json.RawMessage, error) {
	return s.value(ctx, http.MethodGet, accountPoolsPath, nil, "account pool list")
}
func (s Services) GetAccountPool(ctx context.Context, poolID string) (json.RawMessage, error) {
	if err := ValidateResourceID(poolID); err != nil {
		return nil, err
	}
	return s.value(ctx, http.MethodGet, accountPoolsPath+"/"+poolID, nil, "account pool")
}
func (s Services) CreateAccountPool(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return s.value(ctx, http.MethodPost, accountPoolsPath, input, "account pool")
}
func (s Services) UpdateAccountPool(ctx context.Context, poolID, etag string, patch json.RawMessage) (json.RawMessage, error) {
	return s.mutatePool(ctx, http.MethodPatch, poolID, "", etag, patch)
}
func (s Services) ReplaceAccountPoolMembers(ctx context.Context, poolID, etag string, members json.RawMessage) (json.RawMessage, error) {
	return s.mutatePool(ctx, http.MethodPut, poolID, "/members", etag, map[string]json.RawMessage{"members": members})
}
func (s Services) mutatePool(ctx context.Context, method, poolID, suffix, etag string, body any) (json.RawMessage, error) {
	if err := ValidateResourceID(poolID); err != nil {
		return nil, err
	}
	if err := validateStrongETag(etag); err != nil {
		return nil, err
	}
	res, err := s.send(ctx, method, accountPoolsPath+"/"+poolID+suffix, body, etag)
	if err != nil {
		return nil, err
	}
	if method == http.MethodDelete {
		return nil, nil
	}
	return checkedJSON(res.Body, "account pool update")
}
func (s Services) DeleteAccountPool(ctx context.Context, poolID, etag string) error {
	_, err := s.mutatePool(ctx, http.MethodDelete, poolID, "", etag, nil)
	return err
}
func (s Services) ProbeAccountPoolQuota(ctx context.Context, poolID, serviceID string) error {
	if err := ValidateResourceID(poolID); err != nil {
		return err
	}
	if err := ValidateResourceID(serviceID); err != nil {
		return err
	}
	_, err := s.send(ctx, http.MethodPost, accountPoolsPath+"/"+poolID+"/members/"+serviceID+"/quota-probe", struct{}{}, "")
	return err
}

func (t Tokens) UpdateAccessTokenRouting(ctx context.Context, tokenID, etag string, routing json.RawMessage) (json.RawMessage, error) {
	if err := ValidateResourceID(tokenID); err != nil {
		return nil, err
	}
	if err := validateStrongETag(etag); err != nil {
		return nil, err
	}
	res, err := t.send(ctx, http.MethodPatch, "/control/v1/access-tokens/"+tokenID, routing, etag)
	if err != nil {
		return nil, err
	}
	return checkedJSON(res.Body, "access token routing")
}
