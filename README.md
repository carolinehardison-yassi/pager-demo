# LambdaPagerDemo

A Go AWS Lambda function that watches a Google Tasks list named **"Pager"** and emails an alert (via Amazon SES) whenever tasks have been created or updated in the last 10 minutes. It runs on a 5-minute schedule and is also exposed via an API Gateway endpoint for manual invocation.

## How it works

1. Loads a Google OAuth secret (web-client JSON + refresh token) — from AWS Secrets Manager in Lambda, or from a local file when running on your machine.
2. Exchanges the refresh token for a Google access token.
3. Looks up the task list titled `Pager` via the Google Tasks API and fetches tasks updated within the last 10 minutes.
4. If any tasks are found, sends a plain-text alert email through SES (sender and recipient are both `caroline.hardison@yassi.com`, which must be a verified SES identity).
5. Returns a JSON summary (`since`, `recentTasks`, `emailSent`).

## Services involved

| Service | Purpose |
|---|---|
| AWS Lambda (`provided.al2023`, x86_64 Go binary) | Runs the handler (`main/main.go`) |
| Amazon API Gateway | `GET /hello` endpoint for manual invocation |
| Amazon EventBridge (schedule) | Invokes the function every 5 minutes |
| AWS Secrets Manager | Stores the Google OAuth secret as `google/secret3` |
| AWS Parameters and Secrets Lambda Extension (layer) | Local HTTP cache on `localhost:2773` the function uses to read the secret |
| Amazon SES (SESv2) | Sends the alert email |
| Google OAuth 2.0 | Token exchange (`refresh_token` → access token) |
| Google Tasks API | Reads task lists and tasks (`tasks.readonly` scope) |

## Environment variables

| Variable | Where set | Meaning |
|---|---|---|
| `SECRET_SOURCE` | `template.yaml` sets it to `secretsmanager` in Lambda | If `secretsmanager`, the secret is fetched from Secrets Manager via the extension. Any other value (or unset, the local default) reads a secret file instead. |
| `SECRET_FILE` | Optional, local runs | Explicit path to the secret JSON file. If unset, the code tries `cmd/.token/secret.json` (relative to `main/`), then `secret.json`. |
| `AWS_LAMBDA_FUNCTION_NAME` | Set automatically by Lambda | Used to detect Lambda vs. local: locally the handler runs once and prints the result; in Lambda it starts the runtime loop. Don't set this yourself. |
| `AWS_SESSION_TOKEN` | Set automatically by Lambda | Passed as the `X-Aws-Parameters-Secrets-Token` header to authenticate with the secrets extension. |
| `PARAMETERS_SECRETS_EXTENSION_LOG_LEVEL` | `template.yaml` (`debug`) | Log level for the secrets extension layer, useful when troubleshooting secret access. |
| `AWS_PROFILE` / `AWS_REGION` (standard AWS SDK vars) | Local runs | The SES client uses the default AWS credential chain, so local runs need valid AWS credentials in a region where the SES identity is verified (`us-east-1`). |

## Prerequisites

- Go (see `main/go.mod`)
- AWS CLI credentials with permission to call `sesv2:SendEmail` (and Secrets Manager / CloudFormation if deploying)
- AWS SAM CLI (for deploys)
- A Google Cloud project with the Tasks API enabled and an OAuth **web** client whose redirect URI is `http://localhost:8080`
- A Google Tasks list titled `Pager` in the account you authorize
- The alert address verified as an SES identity

## One-time setup: create the OAuth secret

1. Download the OAuth web-client JSON from the Google Cloud console and save it as `main/cmd/.token/secret.json` (see `main/cmd/.token/sample-secret.json` for the expected shape).
2. Run the consent flow to obtain a refresh token:

   ```sh
   cd main
   go run ./cmd/gettoken cmd/.token/secret.json
   ```

   Open the printed URL, sign in as the user whose tasks the Lambda should read, and approve. The tool writes the refresh token back into `secret.json` and prints the full JSON.
3. For deployed runs, store that full JSON as the value of the Secrets Manager secret `google/secret3` (us-east-1).

## Running locally

With `main/cmd/.token/secret.json` in place and AWS credentials configured:

```sh
cd main
go run .
```

Outside Lambda the program invokes the handler once and prints the JSON response. If tasks were updated in the `Pager` list within the last 10 minutes, it will actually send the alert email via SES, so make sure your credentials/region are what you intend.

To point at a secret file somewhere else:

```sh
SECRET_FILE=/path/to/secret.json go run .
```

### Tests

```sh
cd main
go test ./...
```

## Deploying

```sh
sam build
sam deploy
```

Configuration lives in `samconfig.toml` (stack `sam-app`, region `us-east-1`). The build uses `main/Makefile`, which cross-compiles the Linux `bootstrap` binary — note it also copies `cmd/.token/secret.json` into the deployment package, though the deployed function reads from Secrets Manager (`SECRET_SOURCE=secretsmanager`).

The secrets-extension layer ARN is region/architecture specific; it's a template parameter (`SecretsExtensionLayerArn`) overridable in `samconfig.toml` — look up the ARN for your region [here](https://docs.aws.amazon.com/secretsmanager/latest/userguide/retrieving-secrets_lambda.html).

After deploy, the stack outputs the API Gateway URL; hit it to trigger a check manually:

```sh
curl https://<api-id>.execute-api.us-east-1.amazonaws.com/Prod/hello/
```

## Repository layout

```
template.yaml            SAM template (function, API, schedule, IAM policies)
samconfig.toml           SAM deploy config
main/
  main.go                Lambda handler + local entrypoint
  main_test.go           Tests
  Makefile               SAM build target (cross-compiles bootstrap)
  cmd/gettoken/          One-time OAuth consent flow tool
  cmd/.token/            Local secret JSON (never commit the real one)
```

> **Note:** `main/cmd/.token/secret.json` contains real credentials — keep it out of version control.
