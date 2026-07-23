package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// setupTestEnv wires up stubs for the secrets extension, the Google token
// endpoint, and the Tasks API so handler tests can run outside Lambda.
// tasksJSON is what the Tasks API returns for the Pager list's tasks.
func setupTestEnv(t *testing.T, tasksJSON string) {
	t.Helper()

	// Stub Google token endpoint: verify the refresh-token grant.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case r.Form.Get("grant_type") != "refresh_token":
			http.Error(w, "bad grant_type", http.StatusBadRequest)
		case r.Form.Get("client_id") != "test-client-id":
			http.Error(w, "bad client_id", http.StatusUnauthorized)
		case r.Form.Get("client_secret") != "test-client-secret":
			http.Error(w, "bad client_secret", http.StatusUnauthorized)
		case r.Form.Get("refresh_token") != "test-refresh-token":
			http.Error(w, "bad refresh_token", http.StatusUnauthorized)
		default:
			fmt.Fprint(w, `{"access_token":"fake-access-token","expires_in":3599,"token_type":"Bearer"}`)
		}
	}))
	t.Cleanup(tokenSrv.Close)

	// Stub Tasks API.
	tasksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access-token" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/tasks/v1/users/@me/lists":
			fmt.Fprint(w, `{"items":[{"id":"list-1","title":"My Tasks"},{"id":"pager-list-id","title":"Pager"}]}`)
		case "/tasks/v1/lists/pager-list-id/tasks":
			if r.URL.Query().Get("updatedMin") == "" {
				http.Error(w, "expected updatedMin filter", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, tasksJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tasksSrv.Close)

	origBase := TasksAPIBase
	TasksAPIBase = tasksSrv.URL
	t.Cleanup(func() { TasksAPIBase = origBase })

	// Stub secrets extension serving the web-client JSON with the
	// refresh token, token_uri pointing at the stub token endpoint.
	saJSON, err := json.Marshal(map[string]any{
		"web": map[string]string{
			"client_id":     "test-client-id",
			"client_secret": "test-client-secret",
			"token_uri":     tokenSrv.URL,
			"refresh_token": "test-refresh-token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	secretPayload, err := json.Marshal(map[string]string{"SecretString": string(saJSON)})
	if err != nil {
		t.Fatal(err)
	}

	secretsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Aws-Parameters-Secrets-Token") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write(secretPayload)
	}))
	t.Cleanup(secretsSrv.Close)

	origEndpoint := SecretsExtensionEndpoint
	SecretsExtensionEndpoint = secretsSrv.URL
	t.Cleanup(func() { SecretsExtensionEndpoint = origEndpoint })

	t.Setenv("AWS_SESSION_TOKEN", "test-token")
	// Make loadSecret take the Secrets Manager path.
	t.Setenv("SECRET_SOURCE", "secretsmanager")
}

// stubEmail replaces sendAlertEmail and records what was sent.
type stubEmail struct {
	sent    int
	subject string
	body    string
}

func (s *stubEmail) install(t *testing.T) {
	t.Helper()
	orig := sendAlertEmail
	sendAlertEmail = func(ctx context.Context, subject, body string) error {
		s.sent++
		s.subject = subject
		s.body = body
		return nil
	}
	t.Cleanup(func() { sendAlertEmail = orig })
}

func TestLoadSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/secret.json"
	if err := os.WriteFile(path, []byte(`{"web":{"client_id":"local-id"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_FILE", path)

	secret, err := loadSecret()
	if err != nil {
		t.Fatalf("loadSecret failed: %v", err)
	}
	if !strings.Contains(secret, "local-id") {
		t.Fatalf("unexpected secret value: %q", secret)
	}
}

func TestGetSecret(t *testing.T) {
	setupTestEnv(t, `{}`)

	secret, err := getSecret(SecretID)
	if err != nil {
		t.Fatalf("getSecret failed: %v", err)
	}
	if !strings.Contains(secret, "test-client-id") {
		t.Fatalf("unexpected secret value: %q", secret)
	}
}

func TestGetGoogleAccessToken(t *testing.T) {
	t.Run("Exchanges refresh token", func(t *testing.T) {
		setupTestEnv(t, `{}`)

		secret, err := getSecret(SecretID)
		if err != nil {
			t.Fatal(err)
		}
		token, err := getGoogleAccessToken([]byte(secret))
		if err != nil {
			t.Fatalf("getGoogleAccessToken failed: %v", err)
		}
		if token != "fake-access-token" {
			t.Fatalf("unexpected token: %q", token)
		}
	})

	t.Run("Missing refresh token", func(t *testing.T) {
		_, err := getGoogleAccessToken([]byte(`{"web":{"client_id":"x","client_secret":"y"}}`))
		if err == nil || !strings.Contains(err.Error(), "refresh_token") {
			t.Fatalf("expected missing refresh_token error, got: %v", err)
		}
	})
}

func TestHandler(t *testing.T) {
	t.Run("Secrets extension unavailable", func(t *testing.T) {
		t.Setenv("SECRET_SOURCE", "secretsmanager")
		orig := SecretsExtensionEndpoint
		SecretsExtensionEndpoint = "http://127.0.0.1:12345"
		defer func() { SecretsExtensionEndpoint = orig }()

		_, err := handler(context.Background(), events.APIGatewayProxyRequest{})
		if err == nil {
			t.Fatal("Error failed to trigger when secrets extension is unreachable")
		}
	})

	t.Run("Recent tasks trigger an email", func(t *testing.T) {
		setupTestEnv(t, `{"items":[
			{"id":"task-b","title":"Check pager","status":"needsAction","updated":"2026-07-23T09:59:00.000Z"},
			{"id":"task-c","title":"Ack alert","status":"needsAction","updated":"2026-07-23T10:00:00.000Z"}
		]}`)
		email := &stubEmail{}
		email.install(t)

		resp, err := handler(context.Background(), events.APIGatewayProxyRequest{})
		if err != nil {
			t.Fatalf("handler failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("unexpected status: %d", resp.StatusCode)
		}
		if email.sent != 1 {
			t.Fatalf("expected 1 email, got %d", email.sent)
		}
		if email.subject != "Pager: 2 new tasks" {
			t.Fatalf("unexpected subject: %q", email.subject)
		}
		if !strings.Contains(email.body, "Check pager") || !strings.Contains(email.body, "Ack alert") {
			t.Fatalf("email body missing task titles: %s", email.body)
		}
		if !strings.Contains(resp.Body, `"emailSent":true`) {
			t.Fatalf("unexpected response body: %s", resp.Body)
		}
	})

	t.Run("No recent tasks sends no email", func(t *testing.T) {
		setupTestEnv(t, `{"kind":"tasks#tasks","items":[]}`)
		email := &stubEmail{}
		email.install(t)

		resp, err := handler(context.Background(), events.APIGatewayProxyRequest{})
		if err != nil {
			t.Fatalf("handler failed: %v", err)
		}
		if email.sent != 0 {
			t.Fatalf("expected no email, got %d", email.sent)
		}
		if !strings.Contains(resp.Body, `"emailSent":false`) {
			t.Fatalf("unexpected response body: %s", resp.Body)
		}
	})
}
