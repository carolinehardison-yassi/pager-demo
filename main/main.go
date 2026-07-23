package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

var (
	// SecretsExtensionEndpoint is the local endpoint exposed by the
	// AWS Parameters and Secrets Lambda Extension layer.
	SecretsExtensionEndpoint = "http://localhost:2773"

	// SecretID is the Secrets Manager secret holding the Google OAuth
	// web-client JSON plus the refresh_token obtained via cmd/gettoken.
	SecretID = "google/secret3"

	// LocalSecretPath is where the same secret JSON lives when running
	// locally (relative to the main/ directory). Never packaged or
	// deployed — the Makefile ships only the bootstrap binary.
	LocalSecretPath = "cmd/.token/secret.json"

	// TasksAPIBase is the Google Tasks API endpoint.
	TasksAPIBase = "https://tasks.googleapis.com"

	// TaskListTitle is the task list to watch for new tasks.
	TaskListTitle = "Pager"

	// AlertEmail is both the sender and recipient of alert emails.
	// Must be a verified identity in SES.
	AlertEmail = "caroline.hardison@yassi.com"

	// RecentWindow is how far back to look for new/updated tasks.
	RecentWindow = 10 * time.Minute

	// sendAlertEmail delivers the alert; a variable so tests can stub it.
	sendAlertEmail = sendEmailSES
)

// oauthSecret is the stored secret: Google's downloaded web-client JSON
// with a refresh_token added (by cmd/gettoken) after the one-time consent.
type oauthSecret struct {
	Web struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		TokenURI     string `json:"token_uri"`
		RefreshToken string `json:"refresh_token"`
	} `json:"web"`
}

// runningInLambda reports whether we're executing inside AWS Lambda,
// which always sets AWS_LAMBDA_FUNCTION_NAME.
func runningInLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}

// loadSecret returns the Google OAuth secret JSON. By default it reads a
// secret.json file (bundled into the deployment package by the Makefile;
// cmd/.token/secret.json when running locally). Set SECRET_SOURCE=secretsmanager
// to fetch from Secrets Manager via the extension instead.
func loadSecret() (string, error) {
	if os.Getenv("SECRET_SOURCE") == "secretsmanager" {
		return getSecret(SecretID)
	}

	paths := []string{LocalSecretPath, "secret.json"}
	if p := os.Getenv("SECRET_FILE"); p != "" {
		paths = []string{p}
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			return string(data), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("reading secret file %s: %w", p, err)
		}
	}
	return "", fmt.Errorf("no secret file found (tried %v)", paths)
}

// getSecret retrieves a secret from Secrets Manager via the
// AWS Parameters and Secrets Lambda Extension listening on localhost:2773.
func getSecret(secretID string) (string, error) {
	reqURL := fmt.Sprintf("%s/secretsmanager/get?secretId=%s",
		SecretsExtensionEndpoint, url.QueryEscape(secretID))

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Aws-Parameters-Secrets-Token", os.Getenv("AWS_SESSION_TOKEN"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets extension returned status %d (%s): %s",
			resp.StatusCode, resp.Header.Get("X-Amzn-Errortype"), body)
	}

	var out struct {
		SecretString string `json:"SecretString"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.SecretString, nil
}

// getGoogleAccessToken exchanges the stored refresh token for an access
// token acting as the user who granted the original consent.
func getGoogleAccessToken(secretJSON []byte) (string, error) {
	var sec oauthSecret
	if err := json.Unmarshal(secretJSON, &sec); err != nil {
		return "", fmt.Errorf("parsing OAuth client secret JSON: %w", err)
	}
	if sec.Web.ClientID == "" || sec.Web.ClientSecret == "" {
		return "", errors.New("secret is missing web.client_id or web.client_secret")
	}
	if sec.Web.RefreshToken == "" {
		return "", errors.New("secret is missing web.refresh_token — run cmd/gettoken once to authorize and store it")
	}

	resp, err := http.PostForm(sec.Web.TokenURI, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {sec.Web.ClientID},
		"client_secret": {sec.Web.ClientSecret},
		"refresh_token": {sec.Web.RefreshToken},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, body)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("token endpoint response contained no access_token")
	}
	return tok.AccessToken, nil
}

// googleGET performs an authenticated GET and returns the response body.
func googleGET(token, reqURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned status %d: %s", reqURL, resp.StatusCode, body)
	}
	return body, nil
}

// findTaskListID resolves a task list's display title to its ID.
func findTaskListID(token, title string) (string, error) {
	body, err := googleGET(token, TasksAPIBase+"/tasks/v1/users/@me/lists")
	if err != nil {
		return "", err
	}

	var lists struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &lists); err != nil {
		return "", err
	}
	for _, item := range lists.Items {
		if item.Title == title {
			return item.ID, nil
		}
	}
	return "", fmt.Errorf("task list titled %q not found", title)
}

type recentTask struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Updated     string `json:"updated"`
	WebViewLink string `json:"webViewLink"`
}

// recentTasks returns tasks in the list created or updated within RecentWindow.
func recentTasks(token, listID string, since time.Time) ([]recentTask, error) {
	reqURL := fmt.Sprintf("%s/tasks/v1/lists/%s/tasks?showCompleted=true&showHidden=true&updatedMin=%s",
		TasksAPIBase, url.PathEscape(listID), url.QueryEscape(since.UTC().Format(time.RFC3339)))
	body, err := googleGET(token, reqURL)
	if err != nil {
		return nil, err
	}

	var out struct {
		Items []recentTask `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// sendEmailSES sends a plain-text email to AlertEmail via Amazon SES.
func sendEmailSES(ctx context.Context, subject, body string) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}
	client := sesv2.NewFromConfig(cfg)

	_, err = client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(AlertEmail),
		Destination:      &sestypes.Destination{ToAddresses: []string{AlertEmail}},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: aws.String(subject)},
				Body: &sestypes.Body{
					Text: &sestypes.Content{Data: aws.String(body)},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("sending email via SES: %w", err)
	}
	return nil
}

func alertBody(tasks []recentTask, since time.Time) (subject, body string) {
	plural := ""
	if len(tasks) != 1 {
		plural = "s"
	}
	subject = fmt.Sprintf("Pager: %d new task%s", len(tasks), plural)

	var b strings.Builder
	fmt.Fprintf(&b, "%d task%s in the %q list since %s:\n\n",
		len(tasks), plural, TaskListTitle, since.UTC().Format(time.RFC3339))
	for _, t := range tasks {
		fmt.Fprintf(&b, "- %s (status: %s, updated: %s)\n", t.Title, t.Status, t.Updated)
		if t.WebViewLink != "" {
			fmt.Fprintf(&b, "  %s\n", t.WebViewLink)
		}
	}
	return subject, b.String()
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	secret, err := loadSecret()
	if err != nil {
		return events.APIGatewayProxyResponse{}, fmt.Errorf("failed to load secret: %w", err)
	}

	token, err := getGoogleAccessToken([]byte(secret))
	if err != nil {
		return events.APIGatewayProxyResponse{}, fmt.Errorf("failed to get Google access token: %w", err)
	}

	listID, err := findTaskListID(token, TaskListTitle)
	if err != nil {
		return events.APIGatewayProxyResponse{}, err
	}

	since := time.Now().Add(-RecentWindow)
	tasks, err := recentTasks(token, listID, since)
	if err != nil {
		return events.APIGatewayProxyResponse{}, err
	}

	emailSent := false
	if len(tasks) > 0 {
		subject, body := alertBody(tasks, since)
		if err := sendAlertEmail(ctx, subject, body); err != nil {
			return events.APIGatewayProxyResponse{}, err
		}
		emailSent = true
		log.Printf("alert email sent to %s for %d task(s)", AlertEmail, len(tasks))
	} else {
		log.Printf("no tasks in %q since %s; no email sent", TaskListTitle, since.UTC().Format(time.RFC3339))
	}

	respBody, err := json.Marshal(struct {
		Since       string       `json:"since"`
		RecentTasks []recentTask `json:"recentTasks"`
		EmailSent   bool         `json:"emailSent"`
	}{
		Since:       since.UTC().Format(time.RFC3339),
		RecentTasks: tasks,
		EmailSent:   emailSent,
	})
	if err != nil {
		return events.APIGatewayProxyResponse{}, err
	}

	return events.APIGatewayProxyResponse{
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(respBody),
		StatusCode: 200,
	}, nil
}

func main() {
	if !runningInLambda() {
		// Local run: invoke the handler once and print the result.
		resp, err := handler(context.Background(), events.APIGatewayProxyRequest{})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(resp.Body)
		return
	}
	lambda.Start(handler)
}
