BIN=bootstrap
FUNCTION_NAME=cloudtrail-tattletail
FUNCTION_NAME_CWLOGS=cloudtrail-tattletail-cwlogs
S3_SRC=cloudtrail_tattletail.go cloudtrail_tattletail_test.go
CWLOGS_SRC=cloudtrail_tattletail_cwlogs.go

DATE=`date +%Y%m%d`

$(FUNCTION_NAME): $(S3_SRC) $(wildcard **/*.go)
	rm -f $(BIN) $(FUNCTION_NAME).zip
	go test $(S3_SRC)
	go build -o $(BIN) $(S3_SRC)
	zip -r $@.zip $(BIN) $(wildcard ./tattletail.toml)

$(FUNCTION_NAME_CWLOGS): $(CWLOGS_SRC) $(wildcard **/*.go)
	rm -f $(BIN) $(FUNCTION_NAME).zip
	go test $(CWLOGS_SRC)
	go build -o $(BIN) $(CWLOGS_SRC)
	zip -r $@.zip $(BIN) $(wildcard ./tattletail.toml)

.PHONY: upload
upload: $(BIN).zip
	aws lambda update-function-code --function-name $(FUNCTION_NAME) --zip-file fileb://$(BIN).zip
	rm $(BIN).zip

.PHONY: upload_config
upload_config: $(BIN).toml
	aws s3 cp $^ "s3://$(CONF_S3_BUCKET)/$(CONF_S3_PATH)"
