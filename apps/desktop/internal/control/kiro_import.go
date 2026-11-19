package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/QuantumNous/astrlink/core/contract"
)

// ImportKiroFile selects one file in the native host. Neither a caller-supplied
// path nor the tokens cross the WebView bridge. Cancel returns a null result.
func (s Services) ImportKiroFile(ctx context.Context, serviceID string, create *json.RawMessage) (*ServiceRecord, error) {
	if (serviceID == "") != (create != nil) {
		return nil, errors.New("choose one target Kiro service")
	}
	path := ServicesPath
	var fields map[string]json.RawMessage
	if create != nil {
		if json.Unmarshal(*create, &fields) != nil || string(fields["kind"]) != `"kiro_subscription"` {
			return nil, errors.New("invalid Kiro service")
		}
	} else {
		var err error
		path, err = servicePath(serviceID, "/kiro-import")
		if err != nil {
			return nil, err
		}
	}
	file, err := s.PickKiroFile()
	if err != nil || file == "" {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("cannot open the selected Kiro file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("choose a regular JSON file")
	}
	grant, err := readKiroFile(f)
	if err != nil {
		return nil, err
	}
	var body any = grant
	if create != nil {
		raw, err := json.Marshal(grant)
		if err != nil {
			return nil, err
		}
		defer clear(raw)
		fields["kiro_import"] = raw
		body = fields
	}
	record, err := s.record(ctx, http.MethodPost, path, body, "")
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func readKiroFile(input io.Reader) (contract.KiroSocialGrant, error) {
	const limit = 1 << 20
	var grant contract.KiroSocialGrant
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	defer clear(data)
	if err != nil || len(data) > limit {
		return grant, errors.New("cannot read Kiro file (maximum 1 MiB)")
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var accounts []json.RawMessage
		if json.Unmarshal(data, &accounts) != nil || len(accounts) != 1 {
			return grant, errors.New("choose a Kiro JSON export containing one account")
		}
		data = accounts[0]
		defer clear(data)
	}
	if json.Unmarshal(data, &grant) != nil || !grant.Valid() {
		return contract.KiroSocialGrant{}, errors.New("expected a Google or GitHub Kiro export with a refresh token")
	}
	return grant, nil
}
