build-DemoPagerFunction:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bootstrap
	cp ./bootstrap $(ARTIFACTS_DIR)/.
	cp ./cmd/.token/secret.json $(ARTIFACTS_DIR)/secret.json