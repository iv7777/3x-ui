package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithServerBasePath(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.3","info":{"title":"x"},"servers":[{"url":"/","description":"old"}],"paths":{"/p":{"get":{"summary":"s"}}}}`)

	out, err := withServerBasePath(spec, "/test/")
	if err != nil {
		t.Fatalf("withServerBasePath: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	servers, ok := doc["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %v, want one entry", doc["servers"])
	}
	srv, _ := servers[0].(map[string]any)
	if srv["url"] != "/test" {
		t.Errorf("server url = %v, want /test (trailing slash trimmed)", srv["url"])
	}

	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi field not preserved: %v", doc["openapi"])
	}
	if _, ok := doc["paths"].(map[string]any)["/p"]; !ok {
		t.Errorf("paths content not preserved verbatim")
	}
}

func TestWithServerBasePathInvalidJSON(t *testing.T) {
	if _, err := withServerBasePath([]byte("not json"), "/test/"); err == nil {
		t.Errorf("expected error on invalid spec, got nil")
	}
}

func TestPWAHeadInjectionUsesRuntimeBasePath(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		wantPath string
	}{
		{name: "root", basePath: "/", wantPath: "/manifest.webmanifest"},
		{name: "secret path", basePath: "panel-secret", wantPath: "/panel-secret/manifest.webmanifest"},
		{name: "trailing slash", basePath: "/panel-secret/", wantPath: "/panel-secret/manifest.webmanifest"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			head := string(pwaHeadInjection(test.basePath, "login.html"))
			if !strings.Contains(head, `href="`+test.wantPath+`"`) {
				t.Fatalf("manifest URL = %q, want %q", head, test.wantPath)
			}
			if !strings.Contains(head, `src="`+strings.Replace(test.wantPath, "manifest.webmanifest", "pwa-register.js", 1)+`"`) {
				t.Fatalf("registration URL = %q", head)
			}
		})
	}
}

func TestPWAHeadInjectionLinksServedTabIcons(t *testing.T) {
	for _, basePath := range []string{"/", "/panel-secret/"} {
		head := string(pwaHeadInjection(basePath, "index.html"))
		for _, want := range []string{
			`<link rel="icon" type="image/png" sizes="32x32" href="` + basePath + `icons/3x-ui-32.png">`,
			`<link rel="icon" type="image/png" sizes="16x16" href="` + basePath + `icons/3x-ui-16.png">`,
			`<link rel="apple-touch-icon" href="` + basePath + `icons/3x-ui-192.png">`,
		} {
			if !strings.Contains(head, want) {
				t.Fatalf("base %q: head %q lacks %q", basePath, head, want)
			}
			name := strings.TrimPrefix(strings.Split(strings.Split(want, `href="`)[1], `"`)[0], basePath)
			if _, ok := pwaAssets[name]; !ok {
				t.Fatalf("%q is linked but ServePWAIcon would answer 404", name)
			}
		}
	}
}

func TestPWAHeadInjectionSkipsSubscriptionPage(t *testing.T) {
	if got := pwaHeadInjection("/panel-secret/", "subpage.html"); got != nil {
		t.Fatalf("subpage injection = %q, want nil", got)
	}
}
