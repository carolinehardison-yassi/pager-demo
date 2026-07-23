// Command gettoken performs the one-time OAuth consent flow for the Google
// web client and prints the secret JSON (with refresh_token added) to store
// in Secrets Manager.
//
// Usage:
//
//	go run ./cmd/gettoken /path/to/client_secret.json
//
// Sign in as the user whose tasks the Lambda should read.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const scope = "https://www.googleapis.com/auth/tasks.readonly"

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: %s <client_secret.json>", os.Args[0])
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}

	var sec struct {
		Web struct {
			ClientID     string   `json:"client_id"`
			ClientSecret string   `json:"client_secret"`
			AuthURI      string   `json:"auth_uri"`
			TokenURI     string   `json:"token_uri"`
			RedirectURIs []string `json:"redirect_uris"`
		} `json:"web"`
	}
	if err := json.Unmarshal(data, &sec); err != nil {
		log.Fatalf("parsing client secret JSON: %v", err)
	}
	if len(sec.Web.RedirectURIs) == 0 {
		log.Fatal("client secret JSON has no redirect_uris")
	}
	redirectURI := sec.Web.RedirectURIs[0]

	listenAddr := ":8080"
	if u, err := url.Parse(redirectURI); err == nil && u.Port() != "" {
		listenAddr = ":" + u.Port()
	}

	authURL := sec.Web.AuthURI + "?" + url.Values{
		"response_type": {"code"},
		"client_id":     {sec.Web.ClientID},
		"redirect_uri":  {redirectURI},
		"scope":         {scope},
		"access_type":   {"offline"}, // required to receive a refresh_token
		"prompt":        {"consent"}, // force refresh_token even on re-auth
	}.Encode()

	fmt.Println("Open this URL in your browser and sign in as the user whose tasks the Lambda should read:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()

	codeCh := make(chan string, 1)
	srv := &http.Server{Addr: listenAddr}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			http.Error(w, "authorization failed: "+errMsg, http.StatusBadRequest)
			log.Fatalf("authorization failed: %s", errMsg)
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "Authorized — you can close this tab and return to the terminal.")
		codeCh <- code
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	code := <-codeCh
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)

	resp, err := http.PostForm(sec.Web.TokenURI, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {sec.Web.ClientID},
		"client_secret": {sec.Web.ClientSecret},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("token exchange returned status %d: %s", resp.StatusCode, body)
	}

	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		log.Fatal(err)
	}
	if tok.RefreshToken == "" {
		log.Fatal("no refresh_token in response — revoke the app's access at https://myaccount.google.com/permissions and run again")
	}

	// Re-emit the original secret with refresh_token added to the web object.
	var full map[string]any
	if err := json.Unmarshal(data, &full); err != nil {
		log.Fatal(err)
	}
	full["web"].(map[string]any)["refresh_token"] = tok.RefreshToken
	updated, err := json.MarshalIndent(full, "", "  ")
	if err != nil {
		log.Fatal(err)
	}

	if err := os.WriteFile(os.Args[1], updated, 0o600); err != nil {
		log.Fatalf("writing updated secret back to %s: %v", os.Args[1], err)
	}
	fmt.Printf("Updated %s with the refresh token.\n\n", os.Args[1])

	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("Also store this as the FULL value of the Secrets Manager secret:")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(string(updated))
}
