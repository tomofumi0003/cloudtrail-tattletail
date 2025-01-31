package main

import (
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/inconshreveable/log15"
	"github.com/psanford/cloudtrail-tattletail/awsstub"
)

func main() {
	awsstub.InitAWS()
	handler := log15.StreamHandler(os.Stdout, log15.LogfmtFormat())
	log15.Root().SetHandler(handler)
	s := NewServer()
	lambda.Start(s.HandlerS3)
}
