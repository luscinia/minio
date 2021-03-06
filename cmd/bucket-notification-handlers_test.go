/*
 * Minio Cloud Storage, (C) 2016, 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

// Implement a dummy flush writer.
type flushWriter struct {
	io.Writer
}

// Flush writer is a dummy writer compatible with http.Flusher and http.ResponseWriter.
func (f *flushWriter) Flush()                            {}
func (f *flushWriter) Write(b []byte) (n int, err error) { return f.Writer.Write(b) }
func (f *flushWriter) Header() http.Header               { return http.Header{} }
func (f *flushWriter) WriteHeader(code int)              {}

func newFlushWriter(writer io.Writer) http.ResponseWriter {
	return &flushWriter{writer}
}

// Tests write notification code.
func TestWriteNotification(t *testing.T) {
	// Initialize a new test config.
	root, err := newTestConfig(globalMinioDefaultRegion)
	if err != nil {
		t.Fatalf("Unable to initialize test config %s", err)
	}
	defer os.RemoveAll(root)

	var buffer bytes.Buffer
	// Collection of test cases for each event writer.
	testCases := []struct {
		writer http.ResponseWriter
		event  map[string][]NotificationEvent
		err    error
	}{
		// Invalid input argument with writer `nil` - Test - 1
		{
			writer: nil,
			event:  nil,
			err:    errInvalidArgument,
		},
		// Invalid input argument with event `nil` - Test - 2
		{
			writer: newFlushWriter(ioutil.Discard),
			event:  nil,
			err:    errInvalidArgument,
		},
		// Unmarshal and write, validate last 5 bytes. - Test - 3
		{
			writer: newFlushWriter(&buffer),
			event: map[string][]NotificationEvent{
				"Records": {newNotificationEvent(eventData{
					Type:   ObjectCreatedPut,
					Bucket: "testbucket",
					ObjInfo: ObjectInfo{
						Name: "key",
					},
					ReqParams: map[string]string{
						"ip": "10.1.10.1",
					}}),
				},
			},
			err: nil,
		},
	}
	// Validates all the testcases for writing notification.
	for _, testCase := range testCases {
		err := writeNotification(testCase.writer, testCase.event)
		if err != testCase.err {
			t.Errorf("Unable to write notification %s", err)
		}
		// Validates if the ending string has 'crlf'
		if err == nil && !bytes.HasSuffix(buffer.Bytes(), crlf) {
			buf := buffer.Bytes()[buffer.Len()-5 : 0]
			t.Errorf("Invalid suffix found from the writer last 5 bytes %s, expected `\r\n`", string(buf))
		}
		// Not printing 'buf' on purpose, validates look for string '10.1.10.1'.
		if err == nil && !bytes.Contains(buffer.Bytes(), []byte("10.1.10.1")) {
			// Enable when debugging)
			// fmt.Println(string(buffer.Bytes()))
			t.Errorf("Requested content couldn't be found, expected `10.1.10.1`")
		}
	}
}

func TestSendBucketNotification(t *testing.T) {
	// Initialize a new test config.
	root, err := newTestConfig(globalMinioDefaultRegion)
	if err != nil {
		t.Fatalf("Unable to initialize test config %s", err)
	}
	defer os.RemoveAll(root)

	eventCh := make(chan []NotificationEvent)

	// Create a Pipe with FlushWriter on the write-side and bufio.Scanner
	// on the reader-side to receive notification over the listen channel in a
	// synchronized manner.
	pr, pw := io.Pipe()
	fw := newFlushWriter(pw)
	scanner := bufio.NewScanner(pr)
	// Start a go-routine to wait for notification events.
	go func(listenerCh <-chan []NotificationEvent) {
		sendBucketNotification(fw, listenerCh)
	}(eventCh)

	// Construct notification events to be passed on the events channel.
	var events []NotificationEvent
	evTypes := []EventName{
		ObjectCreatedPut,
		ObjectCreatedPost,
		ObjectCreatedCopy,
		ObjectCreatedCompleteMultipartUpload,
	}
	for _, evType := range evTypes {
		events = append(events, newNotificationEvent(eventData{
			Type: evType,
		}))
	}
	// Send notification events to the channel on which sendBucketNotification
	// is waiting on.
	eventCh <- events

	// Read from the pipe connected to the ResponseWriter.
	scanner.Scan()
	notificationBytes := scanner.Bytes()

	// Close the read-end and send an empty notification event on the channel
	// to signal sendBucketNotification to terminate.
	pr.Close()
	eventCh <- []NotificationEvent{}
	close(eventCh)

	// Checking if the notification are the same as those sent over the channel.
	var notifications map[string][]NotificationEvent
	err = json.Unmarshal(notificationBytes, &notifications)
	if err != nil {
		t.Fatal("Failed to Unmarshal notification")
	}
	records := notifications["Records"]
	for i, rec := range records {
		if rec.EventName == evTypes[i].String() {
			continue
		}
		t.Errorf("Failed to receive %d event %s", i, evTypes[i].String())
	}
}

func TestGetBucketNotificationHandler(t *testing.T) {
	ExecObjectLayerAPITest(t, testGetBucketNotificationHandler, []string{
		"GetBucketNotification",
	})
}

func testGetBucketNotificationHandler(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	credentials credential, t *testing.T) {
	// declare sample configs
	filterRules := []filterRule{
		{
			Name:  "prefix",
			Value: "minio",
		},
		{
			Name:  "suffix",
			Value: "*.jpg",
		},
	}
	sampleSvcCfg := ServiceConfig{
		[]string{"s3:ObjectRemoved:*", "s3:ObjectCreated:*"},
		filterStruct{
			keyFilter{filterRules},
		},
		"1",
	}
	sampleNotifCfg := notificationConfig{
		QueueConfigs: []queueConfig{
			{
				ServiceConfig: sampleSvcCfg,
				QueueARN:      "testqARN",
			},
		},
	}
	rec := httptest.NewRecorder()
	req, err := newTestSignedRequestV4("GET", getGetBucketNotificationURL("", bucketName),
		0, nil, credentials.AccessKey, credentials.SecretKey)
	if err != nil {
		t.Fatalf("%s: Failed to create HTTP testRequest for ListenBucketNotification: <ERROR> %v", instanceType, err)
	}
	apiRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Unexpected http response %d", rec.Code)
	}
	if err = persistNotificationConfig(bucketName, &sampleNotifCfg, obj); err != nil {
		t.Fatalf("Unable to save notification config %s", err)
	}
	rec = httptest.NewRecorder()
	req, err = newTestSignedRequestV4("GET", getGetBucketNotificationURL("", bucketName),
		0, nil, credentials.AccessKey, credentials.SecretKey)
	if err != nil {
		t.Fatalf("%s: Failed to create HTTP testRequest for ListenBucketNotification: <ERROR> %v", instanceType, err)
	}
	apiRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Unexpected http response %d", rec.Code)
	}
	notificationBytes, err := ioutil.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("Unexpected error %s", err)
	}
	nConfig := notificationConfig{}
	if err = xml.Unmarshal(notificationBytes, &nConfig); err != nil {
		t.Fatalf("Unexpected XML received %s", err)
	}
	if sampleNotifCfg.QueueConfigs[0].QueueARN != nConfig.QueueConfigs[0].QueueARN {
		t.Fatalf("Uexpected notification configs expected %#v, got %#v", sampleNotifCfg, nConfig)
	}
	if !reflect.DeepEqual(sampleNotifCfg.QueueConfigs[0].Events, nConfig.QueueConfigs[0].Events) {
		t.Fatalf("Uexpected notification configs expected %#v, got %#v", sampleNotifCfg, nConfig)
	}
}

func TestPutBucketNotificationHandler(t *testing.T) {
	ExecObjectLayerAPITest(t, testPutBucketNotificationHandler, []string{
		"PutBucketNotification",
	})
}

func testPutBucketNotificationHandler(obj ObjectLayer, instanceType,
	bucketName string, apiRouter http.Handler, credentials credential,
	t *testing.T) {

	// declare sample configs
	filterRules := []filterRule{
		{
			Name:  "prefix",
			Value: "minio",
		},
		{
			Name:  "suffix",
			Value: "*.jpg",
		},
	}
	sampleSvcCfg := ServiceConfig{
		[]string{"s3:ObjectRemoved:*", "s3:ObjectCreated:*"},
		filterStruct{
			keyFilter{filterRules},
		},
		"1",
	}
	sampleNotifCfg := notificationConfig{
		QueueConfigs: []queueConfig{
			{
				ServiceConfig: sampleSvcCfg,
				QueueARN:      "testqARN",
			},
		},
	}

	{
		sampleNotifCfg.LambdaConfigs = []lambdaConfig{
			{
				sampleSvcCfg, "testLARN",
			},
		}
		xmlBytes, err := xml.Marshal(sampleNotifCfg)
		if err != nil {
			t.Fatalf("%s: Unexpected err: %#v", instanceType, err)
		}
		rec := httptest.NewRecorder()
		req, err := newTestSignedRequestV4("PUT",
			getPutBucketNotificationURL("", bucketName),
			int64(len(xmlBytes)), bytes.NewReader(xmlBytes),
			credentials.AccessKey, credentials.SecretKey)
		if err != nil {
			t.Fatalf("%s: Failed to create HTTP testRequest for PutBucketNotification: <ERROR> %v",
				instanceType, err)
		}
		apiRouter.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Unexpected http response %d", rec.Code)
		}
	}

	{
		sampleNotifCfg.LambdaConfigs = nil
		sampleNotifCfg.TopicConfigs = []topicConfig{
			{
				sampleSvcCfg, "testTARN",
			},
		}
		xmlBytes, err := xml.Marshal(sampleNotifCfg)
		if err != nil {
			t.Fatalf("%s: Unexpected err: %#v", instanceType, err)
		}
		rec := httptest.NewRecorder()
		req, err := newTestSignedRequestV4("PUT",
			getPutBucketNotificationURL("", bucketName),
			int64(len(xmlBytes)), bytes.NewReader(xmlBytes),
			credentials.AccessKey, credentials.SecretKey)
		if err != nil {
			t.Fatalf("%s: Failed to create HTTP testRequest for PutBucketNotification: <ERROR> %v",
				instanceType, err)
		}
		apiRouter.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Unexpected http response %d", rec.Code)
		}
	}
}

func TestListenBucketNotificationNilHandler(t *testing.T) {
	ExecObjectLayerAPITest(t, testListenBucketNotificationNilHandler, []string{
		"ListenBucketNotification",
		"PutObject",
	})
}

func testListenBucketNotificationNilHandler(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	credentials credential, t *testing.T) {
	// get random bucket name.
	randBucket := getRandomBucketName()

	// Nil Object layer
	nilAPIRouter := initTestAPIEndPoints(nil, []string{
		"ListenBucketNotification",
	})
	testRec := httptest.NewRecorder()
	testReq, tErr := newTestSignedRequestV4("GET",
		getListenBucketNotificationURL("", randBucket, []string{},
			[]string{"*.jpg"}, []string{
				"s3:ObjectCreated:*",
				"s3:ObjectRemoved:*",
				"s3:ObjectAccessed:*",
			}), 0, nil, credentials.AccessKey, credentials.SecretKey)
	if tErr != nil {
		t.Fatalf("%s: Failed to create HTTP testRequest for ListenBucketNotification: <ERROR> %v", instanceType, tErr)
	}
	nilAPIRouter.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Test 1: %s: expected HTTP code %d, but received %d: <ERROR> %v",
			instanceType, http.StatusServiceUnavailable, testRec.Code, tErr)
	}
}

func testRemoveNotificationConfig(obj ObjectLayer, instanceType,
	bucketName string, apiRouter http.Handler, credentials credential,
	t *testing.T) {

	invalidBucket := "Invalid\\Bucket"
	// get random bucket name.
	randBucket := bucketName

	nCfg := notificationConfig{
		QueueConfigs: []queueConfig{
			{
				ServiceConfig: ServiceConfig{
					Events: []string{"s3:ObjectRemoved:*",
						"s3:ObjectCreated:*"},
				},
				QueueARN: "testqARN",
			},
		},
	}
	if err := persistNotificationConfig(randBucket, &nCfg, obj); err != nil {
		t.Fatalf("Unexpected error: %#v", err)
	}

	testCases := []struct {
		bucketName  string
		expectedErr error
	}{
		{invalidBucket, BucketNameInvalid{Bucket: invalidBucket}},
		{randBucket, nil},
	}
	for i, test := range testCases {
		tErr := removeNotificationConfig(test.bucketName, obj)
		if tErr != test.expectedErr {
			t.Errorf("Test %d: %s expected error %v, but received %v", i+1, instanceType, test.expectedErr, tErr)
		}
	}
}

func TestRemoveNotificationConfig(t *testing.T) {
	ExecObjectLayerAPITest(t, testRemoveNotificationConfig, []string{
		"PutBucketNotification",
		"ListenBucketNotification",
	})
}
