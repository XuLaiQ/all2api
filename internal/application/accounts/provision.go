package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type ProvisionService struct {
	repository ports.ProvisionRepository
	box        ports.SecretBox
}

func NewProvisionService(repository ports.ProvisionRepository, box ports.SecretBox) *ProvisionService {
	return &ProvisionService{repository: repository, box: box}
}

func (s *ProvisionService) Import(ctx context.Context, channel string, payload map[string]any, actor, ip, idempotency string) (ports.AccountImportResult, error) {
	if channel != "wb" && channel != "doubao" && channel != "chatgpt" {
		return ports.AccountImportResult{}, fmt.Errorf("channel provision is not implemented")
	}
	if s.repository == nil || strings.TrimSpace(idempotency) == "" {
		return ports.AccountImportResult{}, fmt.Errorf("idempotency_key is required")
	}
	if cached, ok, err := s.repository.GetProvisionIdempotency(ctx, channel, "import", idempotency, s.box); err != nil {
		return ports.AccountImportResult{}, err
	} else if ok {
		encoded, _ := json.Marshal(cached.Response)
		var result ports.AccountImportResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			return ports.AccountImportResult{}, err
		}
		return result, nil
	}
	if channel == "doubao" || channel == "chatgpt" {
		imports, err := buildProviderImports(channel, payload)
		if err != nil {
			return ports.AccountImportResult{}, err
		}
		result, err := s.repository.ImportAccounts(ctx, imports, s.box, ports.AuditEvent{Actor: actor, Action: "import_account", IP: ip})
		if err != nil {
			return ports.AccountImportResult{}, err
		}
		encoded, _ := json.Marshal(result)
		var response map[string]any
		_ = json.Unmarshal(encoded, &response)
		if err := s.repository.PutProvisionIdempotency(ctx, channel, "import", idempotency, "", response, float64(time.Now().Add(10*time.Minute).Unix()), s.box); err != nil {
			return ports.AccountImportResult{}, err
		}
		return result, nil
	}
	items, ok := payload["accounts"].([]any)
	if !ok {
		return ports.AccountImportResult{}, fmt.Errorf("accounts must be an array")
	}
	imports := make([]ports.AccountImport, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return ports.AccountImportResult{}, fmt.Errorf("account item must be an object")
		}
		credentials := map[string]string{}
		for _, key := range []string{"access_token", "accessToken", "token", "refresh_token", "refreshToken", "device_token", "deviceToken", "realm", "domain", "uid", "user_id", "userId", "enterprise_id", "enterpriseId"} {
			if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
				canonical := key
				switch key {
				case "accessToken":
					canonical = "access_token"
				case "token":
					canonical = "access_token"
				case "refreshToken":
					canonical = "refresh_token"
				case "deviceToken":
					canonical = "device_token"
				case "user_id", "userId":
					canonical = "uid"
				case "enterpriseId":
					canonical = "enterprise_id"
				}
				credentials[canonical] = value
			}
		}
		uid := credentials["uid"]
		if uid == "" {
			if value, ok := item["account_id"].(string); ok {
				uid = value
			}
		}
		if uid == "" || credentials["access_token"] == "" {
			return ports.AccountImportResult{}, fmt.Errorf("uid and access_token are required")
		}
		realm := credentials["realm"]
		if realm == "" {
			realm = "cn"
		}
		native := realm + ":" + uid
		name := uid
		if value, ok := item["name"].(string); ok && strings.TrimSpace(value) != "" {
			name = value
		}
		imports = append(imports, ports.AccountImport{Channel: channel, ID: "wb:" + native, NativeID: native, Name: name, Kind: "token", Credentials: credentials})
	}
	result, err := s.repository.ImportAccounts(ctx, imports, s.box, ports.AuditEvent{Actor: actor, Action: "import_account", IP: ip})
	if err != nil {
		return ports.AccountImportResult{}, err
	}
	encoded, _ := json.Marshal(result)
	var response map[string]any
	_ = json.Unmarshal(encoded, &response)
	if err := s.repository.PutProvisionIdempotency(ctx, channel, "import", idempotency, "", response, float64(time.Now().Add(10*time.Minute).Unix()), s.box); err != nil {
		return ports.AccountImportResult{}, err
	}
	return result, nil
}

func buildProviderImports(channel string, payload map[string]any) ([]ports.AccountImport, error) {
	values := make([]any, 0)
	if channel == "doubao" {
		if cookie, ok := payload["cookie"].(string); ok && strings.TrimSpace(cookie) != "" {
			values = append(values, map[string]any{"cookie": cookie})
		}
		if accounts, ok := payload["accounts"].([]any); ok {
			values = append(values, accounts...)
		}
	} else {
		if tokens, ok := payload["tokens"].([]any); ok {
			values = append(values, tokens...)
		}
		if accounts, ok := payload["accounts"].([]any); ok {
			values = append(values, accounts...)
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("accounts or token material is required")
	}
	result := make([]ports.AccountImport, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		item := map[string]any{}
		switch value := raw.(type) {
		case string:
			if channel == "doubao" {
				item["cookie"] = value
			} else {
				item["access_token"] = value
			}
		case map[string]any:
			item = value
		default:
			return nil, fmt.Errorf("account item must be a string or object")
		}
		secretKey := "access_token"
		if channel == "doubao" {
			secretKey = "cookie"
		}
		secret, _ := item[secretKey].(string)
		if secret == "" && channel == "chatgpt" {
			secret, _ = item["token"].(string)
		}
		secret = strings.TrimSpace(secret)
		if secret == "" {
			return nil, fmt.Errorf("account item has no credential")
		}
		digest := sha256.Sum256([]byte(secret))
		fingerprint := hex.EncodeToString(digest[:])[:24]
		accountID := fingerprint
		if value, ok := item["account_id"].(string); ok && strings.TrimSpace(value) != "" {
			accountID = strings.TrimSpace(value)
		}
		nativeID := accountID
		if channel == "doubao" {
			nativeID = "cookie:" + accountID
		}
		if channel == "chatgpt" {
			nativeID = "token:" + accountID
		}
		if seen[nativeID] {
			continue
		}
		seen[nativeID] = true
		credentials := map[string]string{}
		if channel == "doubao" {
			credentials["cookie"] = secret
		} else {
			credentials = chatGPTCredentials(item, secret)
			if value, ok := item["account_id"].(string); ok && strings.TrimSpace(value) != "" {
				credentials["chatgpt_account_id"] = strings.TrimSpace(value)
			}
		}
		name := accountID
		if value, ok := item["name"].(string); ok && strings.TrimSpace(value) != "" {
			name = strings.TrimSpace(value)
		}
		result = append(result, ports.AccountImport{Channel: channel, ID: channel + ":" + nativeID, NativeID: nativeID, Name: name, Kind: map[string]string{"doubao": "cookie", "chatgpt": "token"}[channel], Credentials: credentials})
	}
	return result, nil
}

func chatGPTCredentials(item map[string]any, accessToken string) map[string]string {
	credentials := map[string]string{"access_token": strings.TrimSpace(accessToken), "auth_mode": "web"}
	aliases := map[string]string{
		"access_token": "access_token", "accessToken": "access_token", "token": "access_token",
		"refresh_token": "refresh_token", "refreshToken": "refresh_token",
		"id_token": "id_token", "idToken": "id_token", "token_type": "token_type", "tokenType": "token_type",
		"client_id": "client_id", "clientId": "client_id", "auth_mode": "auth_mode", "authMode": "auth_mode",
		"source_type": "source_type", "sourceType": "source_type", "proxy": "proxy", "user_agent": "user_agent",
		"user-agent": "user_agent", "User-Agent": "user_agent", "impersonate": "impersonate",
		"oai_device_id": "oai_device_id", "oai-device-id": "oai_device_id", "device_id": "oai_device_id", "deviceId": "oai_device_id",
		"oai_session_id": "oai_session_id", "oai-session-id": "oai_session_id", "session_id": "oai_session_id", "sessionId": "oai_session_id",
		"chatgpt_account_id": "chatgpt_account_id", "chatgpt-account-id": "chatgpt_account_id",
		"sec_ch_ua": "sec-ch-ua", "sec-ch-ua": "sec-ch-ua", "sec_ch_ua_arch": "sec-ch-ua-arch", "sec-ch-ua-arch": "sec-ch-ua-arch",
		"sec_ch_ua_bitness": "sec-ch-ua-bitness", "sec-ch-ua-bitness": "sec-ch-ua-bitness",
		"sec_ch_ua_full_version": "sec-ch-ua-full-version", "sec-ch-ua-full-version": "sec-ch-ua-full-version",
		"sec_ch_ua_full_version_list": "sec-ch-ua-full-version-list", "sec-ch-ua-full-version-list": "sec-ch-ua-full-version-list",
		"sec_ch_ua_mobile": "sec-ch-ua-mobile", "sec-ch-ua-mobile": "sec-ch-ua-mobile",
		"sec_ch_ua_model": "sec-ch-ua-model", "sec-ch-ua-model": "sec-ch-ua-model",
		"sec_ch_ua_platform": "sec-ch-ua-platform", "sec-ch-ua-platform": "sec-ch-ua-platform",
		"sec_ch_ua_platform_version": "sec-ch-ua-platform-version", "sec-ch-ua-platform-version": "sec-ch-ua-platform-version",
	}
	for source, target := range aliases {
		if value, ok := item[source].(string); ok && strings.TrimSpace(value) != "" {
			credentials[target] = strings.TrimSpace(value)
		}
	}
	credentials["access_token"] = strings.TrimSpace(accessToken)
	for _, source := range []string{"fp", "fingerprint"} {
		if value, ok := item[source].(map[string]any); ok {
			if encoded, err := json.Marshal(value); err == nil {
				credentials["fp"] = string(encoded)
			}
			break
		}
		if value, ok := item[source].(string); ok && strings.TrimSpace(value) != "" {
			credentials["fp"] = strings.TrimSpace(value)
			break
		}
	}
	if credentials["auth_mode"] == "" {
		credentials["auth_mode"] = "web"
	}
	return credentials
}
