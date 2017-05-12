package beater

import (
	"crypto/sha1"
	"fmt"
	"strings"
	"time"

	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/publisher"

	"github.com/sameer1703/csvbeat/beatcsv"
	"github.com/sameer1703/csvbeat/config"

	"encoding/csv"
	"io"
	"path/filepath"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/gosimple/slug"
)

type Csvbeat struct {
	done   chan struct{}
	config config.Config
	client publisher.Client
	state  *beatcsv.StateFile
	beat   *beat.Beat
}

// Creates beater
func New(b *beat.Beat, cfg *common.Config) (beat.Beater, error) {
	config := config.DefaultConfig

	if err := cfg.Unpack(&config); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}

	bt := &Csvbeat{
		done:   make(chan struct{}),
		config: config,
		beat:   b,
	}
	sfConf := map[string]string{
		"filename":     config.StateFileName,
		"filepath":     config.StateFilePath,
		"storage_type": config.StateFileStorageType,
	}

	if config.AwsAccessKey != "" && config.AwsSecretAccessKey != "" && config.AwsS3BucketName != "" {
		sfConf["aws_access_key"] = config.AwsAccessKey
		sfConf["aws_secret_access_key"] = config.AwsSecretAccessKey
		sfConf["aws_s3_bucket_name"] = config.AwsS3BucketName
	}

	sf, err := beatcsv.NewStateFile(sfConf)
	if err != nil {
		logp.Err("Statefile error: %v", err)
		return nil, err
	}

	bt.state = sf

	return bt, nil
}

func (bt *Csvbeat) Run(b *beat.Beat) error {
	logp.Info("csvbeat is running! Hit CTRL-C to stop it.")
	bt.client = b.Publisher.Connect()

	bt.DownloadAndPublish()

	ticker := time.NewTicker(bt.config.Period)
	for {
		select {
		case <-bt.done:
			return nil
		case <-ticker.C:
		}
		bt.DownloadAndPublish()
	}
}

func (bt *Csvbeat) DownloadAndPublish() {
	objects, err := bt.getFilesList()
	if err != nil {
		logp.Err("Error fetching files list: %v", err)
	}
	for _, object := range objects {
		if !bt.state.HasFile(*object.Key) {
			if filepath.Ext(*object.Key) == ".csv" {
				err = bt.processObject(object)
				if err != nil {
					logp.Err("Error processing object: %v", err)
				}
				bt.state.AddFile(*object.Key)
				bt.state.UpdateLastRequestTS(int(time.Now().UTC().Unix()))
				if err := bt.state.Save(); err != nil {
					logp.Info("[ERROR] Could not persist state file to storage: %s", err.Error())
				} else {
					logp.Info("Updated state file")
				}
			} else {
				logp.Warn("Skipping file %s, only csv files supported", *object.Key)
			}
		} else {
			logp.Info("File %s already processed", *object.Key)
		}
	}

}

func (bt *Csvbeat) processObject(object *s3.Object) error {
	reader, err := bt.downloadObject(object)
	if err != nil {
		logp.Err("Error downloading object: %v", err)
	}
	var headers []string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if headers == nil {
			for _, val := range record {
				headers = append(headers, slug.Make(val))
			}
		} else {
			err := bt.processAndPublishRow(headers, record, object.Key)
			if err != nil {
				logp.Err("Error processing row: %v", err)
			}
		}

	}
	return nil
}
func (bt *Csvbeat) processAndPublishRow(headers []string, record []string, filename *string) error {
	event := common.MapStr{}
	var timestampstring time.Time
	for i, header := range headers {
		if header == "domain" {
			dateDomain := strings.Split(record[i], " ")
			dateParams := strings.Split(dateDomain[0], "/")
			timeParams := strings.Split(dateDomain[1], ":")
			year, _ := strconv.Atoi(dateParams[2])
			month, _ := strconv.Atoi(dateParams[0])
			day, _ := strconv.Atoi(dateParams[1])
			hour, _ := strconv.Atoi(timeParams[0])
			mins, _ := strconv.Atoi(timeParams[1])
			secs, _ := strconv.Atoi(timeParams[2])
			timestampstring = time.Date(year, time.Month(month), day, hour, mins, secs, 0, time.UTC)
		}
		if header == bt.config.EventTypeColumn {
			event["type"] = record[i]
		}
		if val, err := strconv.ParseInt(record[i], 10, 64); err == nil {
			event[header] = val
		} else if val, err := strconv.ParseFloat(record[i], 64); err == nil {
			event[header] = val
		} else {
			event[header] = record[i]
		}
	}

	event["@timestamp"] = common.Time(timestampstring)
	event["type"] = "transaction"
	event["filename"] = filename
	event["key"] = bt.getRowKey(event)
	bt.client.PublishEvent(event)
	logp.Info("Event sent")

	return nil
}

func (bt *Csvbeat) getRowKey(event common.MapStr) string {
	key, _ := bt.beat.Config.Output["logstash"].String("index", 0)
	key = fmt.Sprintf("%s_%s_%s_%s", key, event["domain"], event["host"], event["metricname"])
	h := sha1.New()
	io.WriteString(h, key)
	return fmt.Sprintf("%x", h.Sum(nil)) //b64.StdEncoding.EncodeToString([]byte(key))
}

func (bt *Csvbeat) downloadObject(object *s3.Object) (*csv.Reader, error) {
	svc, err := bt.getAwsSession()
	if err != nil {
		return nil, err
	}
	params := &s3.GetObjectInput{
		Bucket: aws.String(bt.config.AwsS3BucketName),
		Key:    aws.String(*object.Key),
	}

	resp, err := svc.GetObject(params)
	if err != nil {
		return nil, err
	}

	//defer resp.Body.Close()

	reader := csv.NewReader(resp.Body)
	return reader, nil

}

func (bt *Csvbeat) getFilesList() ([]*s3.Object, error) {
	svc, err := bt.getAwsSession()
	if err != nil {
		return nil, err
	}
	params := &s3.ListObjectsInput{
		Bucket: aws.String(bt.config.AwsS3BucketName),
		Prefix: aws.String(bt.config.FilesPrefix),
	}

	resp, err := svc.ListObjects(params)
	if err != nil {
		return nil, err
	}

	for _, object := range resp.Contents {
		logp.Info("Object: %s", *object.Key)
	}
	return resp.Contents, nil
}

func (bt *Csvbeat) getAwsSession() (*s3.S3, error) {
	sess := session.New(&aws.Config{
		Region: aws.String(bt.config.AwsRegion),
	})
	token := ""
	creds := credentials.NewStaticCredentials(bt.config.AwsAccessKey, bt.config.AwsSecretAccessKey, token)
	_, err := creds.Get()
	if err != nil {
		logp.Info("[ERROR] AWS Credentials: %v", err)
		return nil, err
	}
	svc := s3.New(sess, &aws.Config{
		Region:      aws.String(bt.config.AwsRegion),
		Credentials: creds,
	})

	return svc, nil
}

func (bt *Csvbeat) Stop() {
	if err := bt.state.Save(); err != nil {
		logp.Info("[ERROR] Could not persist state file to storage while shutting down: %s", err.Error())
	}

	bt.client.Close()
	close(bt.done)
}
