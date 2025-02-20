BIN=bootstrap
FUNCTION_NAME_S3=cloudtrail-tattletail-s3
FUNCTION_NAME_CWLOGS=cloudtrail-tattletail-cwlogs
FUNCTION_NAME_S3_SRC=cloudtrail_tattletail_s3.go cloudtrail_tattletail_main.go cloudtrail_tattletail_s3_test.go
FUNCTION_NAME_CWLOGS_SRC=cloudtrail_tattletail_cwlogs.go cloudtrail_tattletail_main.go
CONF_S3_BUCKET=
CONF_S3_PATH=

VERSION=1.0.0

$(FUNCTION_NAME_S3): $(FUNCTION_NAME_S3_SRC) $(wildcard **/*.go)
	rm -f $(BIN) $@.zip
	go test $(FUNCTION_NAME_S3_SRC)
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) $(FUNCTION_NAME_S3_SRC)
	zip -r $@.zip $(BIN) $(wildcard ./tattletail.toml)

$(FUNCTION_NAME_CWLOGS): $(FUNCTION_NAME_CWLOGS_SRC) $(wildcard **/*.go)
	rm -f $(BIN) $@.zip
	go test $(FUNCTION_NAME_CWLOGS_SRC)
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) $(FUNCTION_NAME_CWLOGS_SRC)
	zip -r $@.zip $(BIN) $(wildcard ./tattletail.toml)

.PHONY: upload
upload: $(BIN).zip
	aws lambda update-function-code --function-name $(FUNCTION_NAME) --zip-file fileb://$(BIN).zip
	rm $(BIN).zip

.PHONY: upload_config
upload_config: $(BIN).toml
	aws s3 cp $^ "s3://$(CONF_S3_BUCKET)/$(CONF_S3_PATH)"
