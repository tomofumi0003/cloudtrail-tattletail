package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
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

var version string

func NewServer() *server {
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

	general General
}

func (s *server) HandlerS3(evt events.S3Event) error {
	lgr := log15.New()

	if version != "" {
		lgr.Info("cloudtrail_tattletail", "version", version)
	}

	err := s.loadConfig(lgr)
	if err != nil {
		return err
	}

	for _, rec := range evt.Records {
		err := s.handleRecordS3(lgr, rec)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *server) HandlerCWLogs(evt events.CloudwatchLogsEvent) error {
	lgr := log15.New()

	if version != "" {
		lgr.Info("cloudtrail_tattletail", "version", version)
	}

	err := s.loadConfig(lgr)
	if err != nil {
		return err
	}

	err = s.handleRecordCWLogs(lgr, evt.AWSLogs)
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

	s.general.timezone = conf.General.TimeZone
	s.general.keys = conf.General.Keys
	if conf.General.Version != "" {
		lgr.Info("rule_version", "version", conf.General.Version)
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
		r.datatype = "jsonObj"
		if rule.ResultDataType != "" {
			r.datatype = rule.ResultDataType
		}
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

func (s *server) handleRecordS3(lgr log15.Logger, s3rec events.S3EventRecord) error {
	bucket := s3rec.S3.Bucket.Name
	file := s3rec.S3.Object.Key

	lgr = lgr.New("cloudtrail_file", file)
	lgr.Info("process_file")

	getInput := s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &file,
	}

	dontAutoInflate := func(r *request.Request) {
		r.HTTPRequest.Header.Add("Accept-Encoding", "gzip")
	}

	resp, err := awsstub.S3GetObjWithContext(context.Background(), &getInput, dontAutoInflate)
	if err != nil {
		lgr.Error("s3_fetch_err", "err", err)
		return err
	}
	defer resp.Body.Close()

	r, err := gzip.NewReader(resp.Body)
	if err != nil {
		lgr.Error("new_gz_reader_err", "err", err)
		return err
	}
	body, err := ioutil.ReadAll(r)
	if err != nil {
		lgr.Error("read_body_err", "err", err)
		return err
	}

	var doc struct {
		Records []map[string]interface{} `json:"records"`
	}

	err = json.Unmarshal(body, &doc)
	if err != nil {
		lgr.Error("decode_json_err", "err", err)
		return err
	}

	var matchCount int

	for _, rec := range doc.Records {
		rec = s.modifyRecord(lgr, rec)
		for _, rule := range s.rules {
			var evtID string
			idI, ok := rec["eventID"]
			if ok {
				evtID, _ = idI.(string)
			}
			if match, resultData := rule.Match(lgr, rec); match {
				matchCount++
				lgr.Info("rule_matched", "rule_name", rule.name, "evt_id", evtID)
				for _, dest := range rule.dests {
					lgr.Info("publish_alert", "dest", dest, "rule_name", rule.name, "evt_id", evtID)
					obj := resultData
					if rule.datatype == "jsonStr" {
						err = json.Unmarshal([]byte(resultData.(string)), &obj)
						if err != nil {
							lgr.Info("json_unmarshal_err", "err", err, "result_data", resultData)
							obj = resultData
						}
					}
					err = dest.Send(rule.name, rule.desc, rec, obj)
					if err != nil {
						lgr.Error("publish_alert_err", "err", err, "type", dest.Type(), "rule_name", rule.name, "evt_id", evtID)
					}
				}
			}
		}
	}

	lgr.Info("processing_complete", "record_count", len(doc.Records), "match_count", matchCount)

	return nil
}

func (s *server) handleRecordCWLogs(lgr log15.Logger, rawdata events.CloudwatchLogsRawData) error {
	data, err := rawdata.Parse()
	if err != nil {
		lgr.Error("decode_json_err", "err", err)
		return err
	}
	// lgr.Debug("CloudwatchLogsData", "data", data)

	var matchCount int

	for _, logEvent := range data.LogEvents {
		lgr.Debug("CloudwatchLogsLogEvent", "message", logEvent.Message)

		var rec map[string]interface{}

		err = json.Unmarshal([]byte(logEvent.Message), &rec)
		if err != nil {
			lgr.Error("decode_json_err", "err", err)
			return err
		}

		rec = s.modifyRecord(lgr, rec)

		for _, rule := range s.rules {
			var evtID string
			idI, ok := rec["eventID"]
			if ok {
				evtID, _ = idI.(string)
			}
			if match, resultData := rule.Match(lgr, rec); match {
				matchCount++
				lgr.Info("rule_matched", "rule_name", rule.name, "evt_id", evtID)
				for _, dest := range rule.dests {
					lgr.Info("publish_alert", "dest", dest, "rule_name", rule.name, "evt_id", evtID)
					obj := resultData
					if rule.datatype == "jsonStr" {
						err = json.Unmarshal([]byte(resultData.(string)), &obj)
						if err != nil {
							lgr.Info("json_unmarshal_err", "err", err, "result_data", resultData)
							obj = resultData
						}
					}
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

func (s *server) modifyRecord(lgr log15.Logger, rec map[string]interface{}) map[string]interface{} {
	// lgr.Info("general_conf", "general", s.general)
	tz, err := time.LoadLocation("")
	if s.general.timezone != "" {
		tz, err = time.LoadLocation(s.general.timezone)
		if err != nil {
			lgr.Info("timezone_set_err", "err", err)
		}
	}

	for _, key := range s.general.keys {
		q, err := gojq.Parse(key)
		if err != nil {
			lgr.Error("record_parse_err", "err", err)
			break
		}
		iter := q.Run(rec)
		v, ok := iter.Next()
		if !ok || v == nil {
			lgr.Info("rec_no_key", "key", key, "err", ok)
			break
		}

		lgr.Info("orgTime", "key", key, "time", v)
		parsedTime, err := time.Parse(time.RFC3339, v.(string))
		if err != nil {
			lgr.Error("time_parse_err", "err", err)
			break
		}
		tzParsedTime := parsedTime.In(tz).Format(time.RFC3339)

		q, err = gojq.Parse(key + " |= \"" + tzParsedTime + "\"")
		if err != nil {
			lgr.Error("record_parse_err", "err", err)
			break
		}
		lgr.Info("newTime", "key", key, "time", tzParsedTime, "timezone", tz.String())
		iter = q.Run(rec)
		v, ok = iter.Next()
		if !ok || v == nil {
			lgr.Info("rec_no_key", "key", key, "err", ok)
			break
		}
		rec = v.(map[string]interface{})
	}

	return rec
}

type Rule struct {
	name      string
	desc      string
	query     *gojq.Query
	datatype  string
	transform *gojq.Query
	dests     []destination.Destination
}

type General struct {
	timezone  string
	keys      []string
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
