package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/inconshreveable/log15"
	"github.com/itchyny/gojq"
	"github.com/psanford/cloudtrail-tattletail/awsstub"
	"github.com/psanford/cloudtrail-tattletail/config"
	"github.com/psanford/cloudtrail-tattletail/internal/destination"
	"github.com/psanford/cloudtrail-tattletail/internal/destses"
	"github.com/psanford/cloudtrail-tattletail/internal/destslack"
	"github.com/psanford/cloudtrail-tattletail/internal/destsns"
)

func main() {
	awsstub.InitAWS()
	handler := log15.StreamHandler(os.Stdout, log15.LogfmtFormat())
	log15.Root().SetHandler(handler)
	s := newServer()
	lambda.Start(s.Handler)
}

func newServer() *server {
	loaders := []destination.Loader{
		destsns.NewLoader(),
		destses.NewLoader(),
		destslack.NewLoader(),
	}

	s := server{
		loaders: make(map[string]destination.Loader),
	}
	for _, l := range loaders {
		s.loaders[l.Type()] = l
	}
	return &s
}

type server struct {
	loaders map[string]destination.Loader

	rules []Rule
}

func (s *server) Handler(evt events.CloudwatchLogsEvent) error {
	lgr := log15.New()

	err := s.loadConfig(lgr)
	if err != nil {
		return err
	}

	err = s.handleRecord(lgr, evt.AWSLogs)
	if err != nil {
		return err
	}

	return nil
}

func (s *server) loadConfig(lgr log15.Logger) error {
	// if S3_CONFIG_BUCKET and S3_CONFIG_PATH are set, load config file from s3
	bucketName := os.Getenv("S3_CONFIG_BUCKET")
	confPath := os.Getenv("S3_CONFIG_PATH")

	var confReader io.Reader

	if bucketName != "" && confPath != "" {
		lgr = lgr.New("conf_src", "s3", "bucket", bucketName, "path", confPath)
		confResp, err := awsstub.S3GetObj(&s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(confPath),
		})

		if err != nil {
			lgr.Error("load_config_from_s3_err", "err", err)
			return err
		}
		defer confResp.Body.Close()

		confReader = confResp.Body
	} else {
		fname := "tattletail.toml"
		lgr = lgr.New("conf_src", "local_bundle", "filename", fname)
		f, err := os.Open(fname)
		if err != nil {
			lgr.Error("open_bundled_config_file_err", "err", err)
			return err
		}
		defer f.Close()
		confReader = f
	}

	var conf config.Config
	_, err := toml.DecodeReader(confReader, &conf)
	if err != nil {
		lgr.Error("config_toml_parse_err", "err", err)
		return err
	}

	destinations := make(map[string]destination.Destination)

	for _, dest := range conf.Destinations {
		loader := s.loaders[dest.Type]
		if loader == nil {
			lgr.Error("invalid_destination_type", "id", dest.ID, "invalid_type", dest.Type)
			return fmt.Errorf("invalid destination type %q for destination %q", dest.Type, dest.ID)
		}

		d, err := loader.Load(dest)
		if err != nil {
			lgr.Error("invalid_destination_config", "err", err)
			return err
		}

		if _, exists := destinations[d.ID()]; exists {
			lgr.Error("duplicate_destinations_with_same_id", "id", d.ID())
			return fmt.Errorf("duplicate destinations with same id: %q", d.ID())
		}

		destinations[d.ID()] = d
	}

	s.rules = make([]Rule, 0, len(conf.Rules))

	for i, rule := range conf.Rules {
		r := Rule{
			name: rule.Name,
			desc: rule.Desc,
		}
		if rule.JQMatch == "" {
			lgr.Error("jq_match_not_defined_for_rule", "rule_name", rule.Name, "rule_idx", i)
			return fmt.Errorf("jq_match not defined for rule name=%q idx=%d", rule.Name, i)
		}
		q, err := gojq.Parse(rule.JQMatch)
		if err != nil {
			return fmt.Errorf("parse jq_match err for rule name=%q idx=%d query=%q err=%w", rule.Name, i, rule.JQMatch, err)
		}
		r.query = q

		for _, destName := range rule.Destinations {
			dest := destinations[destName]
			if dest == nil {
				return fmt.Errorf("unknown destination %q for rule name=%q idx=%d", destName, r.name, i)
			}
			r.dests = append(r.dests, dest)
		}

		s.rules = append(s.rules, r)
		lgr.Info("loaded_rule", "name", r.name)
	}

	return nil
}

func (s *server) handleRecord(lgr log15.Logger, rawdata events.CloudwatchLogsRawData) error {
	data, err := rawdata.Parse()
	if err != nil {
		lgr.Error("decode_json_err", "err", err)
		return err
	}

	// lgr.Debug("CloudwatchLogsData", "data", data)

	var matchCount int

	for _, logEvent := range data.LogEvents {
		lgr.Debug("CloudwatchLogsLogEvent", "data", logEvent.Message)

		var rec map[string]interface{}

		err = json.Unmarshal([]byte(logEvent.Message), &rec)
		if err != nil {
			lgr.Error("decode_json_err", "err", err)
			return err
		}

		for _, rule := range s.rules {
			var evtID string
			idI, ok := rec["eventID"]
			if ok {
				evtID, _ = idI.(string)
			}
			if match, obj := rule.Match(lgr, rec); match {
				matchCount++
				lgr.Info("rule_matched", "rule_name", rule.name, "evt_id", evtID)
				for _, dest := range rule.dests {
					lgr.Info("publish_alert", "dest", dest, "rule_name", rule.name, "evt_id", evtID)
					err = dest.Send(rule.name, rule.desc, rec, obj)
					if err != nil {
						lgr.Error("publish_alert_err", "err", err, "type", dest.Type(), "rule_name", rule.name, "evt_id", evtID)
					}
				}
			}
		}
	}

	lgr.Info("processing_complete", "record_count", len(data.LogEvents), "match_count", matchCount)

	return nil
}

type Rule struct {
	name      string
	desc      string
	query     *gojq.Query
	transform *gojq.Query
	dests     []destination.Destination
}

func (r *Rule) Match(lgr log15.Logger, rec map[string]interface{}) (bool, interface{}) {
	iter := r.query.Run(rec)
	v, ok := iter.Next()
	if !ok {
		return false, nil
	}
	if err, ok := v.(error); ok {
		lgr.Error("match_err", "err", err, "rule_name", r.name, "obj", rec)
		return false, ""
	}

	if v == nil {
		return false, nil
	}

	if bval, ok := v.(bool); ok {
		// if the query evaluates to a bool, we say we match if it is true
		return bval, bval
	}

	return true, v
}
