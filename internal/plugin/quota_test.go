package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/ThunderHammerKing/opencode-go-cpa/internal/config"
	"github.com/ThunderHammerKing/opencode-go-cpa/resources"
)

// okHostResult is the envelope a successful host callback returns.
func okHostResult() []byte {
	return []byte(`{"ok":true,"result":{}}`)
}

func TestManagementRegistration(t *testing.T) {
	m := NewManager(nil)
	var got struct {
		Routes    []struct{ Method, Path string }            `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`)), &got)
	// login-submit is a management route (resource routes are GET-only
	// host-side); /quota is the sidebar menu, /quota/data and /login are
	// registered with empty Menu to stay out of the sidebar.
	if len(got.Routes) != 1 || got.Routes[0].Method != http.MethodPost || got.Routes[0].Path != "/plugins/"+pluginName+"/login-submit" {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Resources) != 3 {
		t.Fatalf("resources = %+v", got.Resources)
	}
	for _, r := range got.Resources {
		if r.Path == "/quota" && r.Menu != "OpenCode Go Quota" {
			t.Fatalf("quota menu = %+v", got.Resources)
		}
	}
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &registration)
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("registration did not advertise management_api")
	}
	if registration.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeBoth {
		t.Fatalf("executor scope = %q, want both", registration.Capabilities.ExecutorModelScope)
	}
}

func TestQuotaPageAndDataServed(t *testing.T) {
	m := NewManager(nil)
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: "quota-key-a"}}}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/quota",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Headers.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || len(resp.Body) == 0 {
		t.Fatalf("quota page content-type=%q len=%d", ct, len(resp.Body))
	}
	resp, err = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/quota/data",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Credentials []struct {
			AuthID string `json:"auth_id"`
			Label  string `json:"label"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	id, label := quotaIdentity("quota-key-a")
	if len(got.Credentials) != 1 || got.Credentials[0].AuthID != id || got.Credentials[0].Label != label {
		t.Fatalf("credentials = %+v", got.Credentials)
	}
}

func TestLoginPageServed(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/login",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Headers.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || len(resp.Body) == 0 {
		t.Fatalf("login page content-type=%q len=%d", ct, len(resp.Body))
	}
}

func TestLoginSubmitRequiresLiveState(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		return okHostResult(), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	// Unknown state is refused.
	resp, err := m.handleLoginSubmit(context.Background(), []byte(`{"state":"deadbeef","api_key":"sk-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), "expired") {
		t.Fatalf("unknown state body = %s", resp.Body)
	}
}

func TestLoginFlowEndToEnd(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		return okHostResult(), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	ap := authProvider{}
	start, err := ap.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{
		BaseURL: "http://127.0.0.1:8317/v0/management/oauth-callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(start.URL, "/v0/resource/plugins/"+pluginName+"/login?state=") {
		t.Fatalf("login url = %q", start.URL)
	}
	// The page posts literal JSON keys, so build the body literally to keep
	// the contract honest.
	resp, err := m.handleLoginSubmit(context.Background(), []byte(`{"state":"`+start.State+`","api_key":"sk-login-secret","label":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), `"status":"ok"`) {
		t.Fatalf("submit body = %s", resp.Body)
	}
	poll, err := ap.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{Provider: ProviderID, State: start.State})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %q, want success", poll.Status)
	}
	if !strings.HasPrefix(poll.Auth.ID, "opencode-go-key-") || poll.Auth.Attributes["api_key"] != "sk-login-secret" {
		t.Fatalf("poll auth = %+v", poll.Auth)
	}
	// The credential was persisted through the host.
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) == 0 {
		t.Fatal("login submit did not save the auth record")
	}
}

func TestQuotaPageIsSecretFree(t *testing.T) {
	for name, page := range map[string][]byte{"quota": resources.QuotaPage, "login": resources.LoginPage} {
		if len(page) == 0 {
			t.Fatalf("%s page is empty", name)
		}
		if strings.Contains(string(page), testKey) {
			t.Fatalf("%s page embeds key material", name)
		}
	}
}
