package http_response

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/testutil"
)

// Receives a list with fields that are expected to be absent
func checkAbsentFields(t *testing.T, fields []string, acc *testutil.Accumulator) {
	for _, field := range fields {
		ok := acc.HasField("http_response", field)
		require.False(t, ok)
	}
}

// Receives a list with tags that are expected to be absent
func checkAbsentTags(t *testing.T, tags []string, acc *testutil.Accumulator) {
	for _, tag := range tags {
		ok := acc.HasTag("http_response", tag)
		require.False(t, ok)
	}
}

// Receives a dictionary and with expected fields and their values. If a value is nil, it will only check
// that the field exists, but not its contents
func checkFields(t *testing.T, fields map[string]interface{}, acc *testutil.Accumulator) {
	t.Helper()
	for key, field := range fields {
		switch v := field.(type) {
		case int:
			value, ok := acc.IntField("http_response", key)
			require.True(t, ok)
			require.Equal(t, field, value)
		case float64:
			value, ok := acc.FloatField("http_response", key)
			require.True(t, ok)
			require.InDelta(t, field, value, testutil.DefaultDelta)
		case string:
			value, ok := acc.StringField("http_response", key)
			require.True(t, ok)
			require.Equal(t, field, value)
		case nil:
			ok := acc.HasField("http_response", key)
			require.True(t, ok)
		default:
			t.Log("Unsupported type for field: ", v)
			t.Fail()
		}
	}
}

// Receives a dictionary and with expected tags and their values. If a value is nil, it will only check
// that the tag exists, but not its contents
func checkTags(t *testing.T, tags map[string]interface{}, acc *testutil.Accumulator) {
	for key, tag := range tags {
		switch v := tag.(type) {
		case string:
			ok := acc.HasTag("http_response", key)
			require.True(t, ok)
			require.Equal(t, tag, acc.TagValue("http_response", key))
		case nil:
			ok := acc.HasTag("http_response", key)
			require.True(t, ok)
		default:
			t.Log("Unsupported type for tag: ", v)
			t.Fail()
		}
	}
}

func setUpTestMux() http.Handler {
	mux := http.NewServeMux()
	// Ignore all returned errors below as the tests will fail anyway
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/good", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/good", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "MyTestServer")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, "hit the good page!")
	})
	mux.HandleFunc("/form", func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		defer req.Body.Close()
		if err != nil {
			http.Error(w, "couldn't read request body", http.StatusBadRequest)
			return
		}
		if string(body) != "list=foobar&list=fizbuzz&test=42" {
			fmt.Println(string(body))
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/invalidUTF8", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte{0xff, 0xfe, 0xfd}) //nolint:errcheck // ignore the returned error as the test will fail anyway
	})
	mux.HandleFunc("/noheader", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hit the good page!")
	})
	mux.HandleFunc("/jsonresponse", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "\"service_status\": \"up\", \"healthy\" : \"true\"")
	})
	mux.HandleFunc("/badredirect", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/badredirect", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/mustbepostmethod", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "method wasn't post", http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprintf(w, "used post correctly!")
	})
	mux.HandleFunc("/musthaveabody", func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		defer req.Body.Close()
		if err != nil {
			http.Error(w, "couldn't read request body", http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			http.Error(w, "body was empty", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "sent a body!")
	})
	mux.HandleFunc("/twosecondnap", func(http.ResponseWriter, *http.Request) {
		time.Sleep(time.Second * 2)
	})
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func checkOutput(t *testing.T, acc *testutil.Accumulator, presentFields, presentTags map[string]interface{}, absentFields, absentTags []string) {
	t.Helper()
	if presentFields != nil {
		checkFields(t, presentFields, acc)
	}

	if presentTags != nil {
		checkTags(t, presentTags, acc)
	}

	if absentFields != nil {
		checkAbsentFields(t, absentFields, acc)
	}

	if absentTags != nil {
		checkAbsentTags(t, absentTags, acc)
	}
}

func TestHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cHeader := r.Header.Get("Content-Type")
		uaHeader := r.Header.Get("User-Agent")
		require.Equal(t, "Hello", r.Host)
		require.Equal(t, "application/json", cHeader)
		require.Equal(t, internal.ProductToken(), uaHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL},
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 2),
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Host":         "Hello",
		},
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestFields(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestResponseBodyField(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		ResponseBodyField: "my_body_field",
		FollowRedirects:   true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
		"my_body_field":      "hit the good page!",
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	// Invalid UTF-8 String
	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/invalidUTF8"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		ResponseBodyField: "my_body_field",
		FollowRedirects:   true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"result_type": "body_read_error",
		"result_code": 2,
	}
	expectedTags = map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "body_read_error",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestResponseBodyFormField(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:  testutil.Logger{},
		URLs: []string{ts.URL + "/form"},
		BodyForm: map[string][]string{
			"test": {"42"},
			"list": {"foobar", "fizbuzz"},
		},
		Method: "POST",
		Headers: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
		ResponseTimeout:   config.Duration(time.Second * 20),
		ResponseBodyField: "my_body_field",
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
		"my_body_field":      "",
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "POST",
		"status_code": "200",
		"result":      "success",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestResponseBodyMaxSize(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		ResponseBodyMaxSize: config.Size(5),
		FollowRedirects:     true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"result_type": "body_read_error",
		"result_code": 2,
	}
	expectedTags := map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "body_read_error",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestHTTPHeaderTags(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		HTTPHeaderTags:  map[string]string{"Server": "my_server", "Content-Type": "content_type"},
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":       nil,
		"method":       "GET",
		"status_code":  "200",
		"result":       "success",
		"my_server":    "MyTestServer",
		"content_type": "application/json; charset=utf-8",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/noheader"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		HTTPHeaderTags:  map[string]string{"Server": "my_server", "Content-Type": "content_type"},
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedTags = map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	// Connection failed
	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{"https:/nonexistent.nonexistent"}, // Any non-routable IP works here
		Body:            "",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 5),
		HTTPHeaderTags:  map[string]string{"Server": "my_server", "Content-Type": "content_type"},
		FollowRedirects: false,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"result_type": "connection_failed",
		"result_code": 3,
	}
	expectedTags = map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "connection_failed",
	}
	absentFields = []string{"http_response_code", "response_time", "content_length", "response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func findInterface() (net.Interface, error) {
	potential, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, err
	}

	for _, i := range potential {
		// we are only interest in loopback interfaces which are up
		if (i.Flags&net.FlagUp == 0) || (i.Flags&net.FlagLoopback == 0) {
			continue
		}

		if addrs, err := i.Addrs(); err == nil && len(addrs) > 0 {
			return i, nil
		}
	}

	return net.Interface{}, errors.New("cannot find suitable loopback interface")
}

func TestInterface(t *testing.T) {
	var (
		mux = setUpTestMux()
		ts  = httptest.NewServer(mux)
	)

	defer ts.Close()

	intf, err := findInterface()
	require.NoError(t, err)

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
		Interface:       intf.Name,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestRedirects(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/redirect"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/badredirect"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"result_type": "connection_failed",
		"result_code": 3,
	}
	expectedTags = map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "connection_failed",
	}
	absentFields = []string{"http_response_code", "response_time", "response_string_match"}
	absentTags := []string{"status_code"}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)

	expectedFields = map[string]interface{}{"result_type": "connection_failed"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, absentTags)
}

func TestMethod(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/mustbepostmethod"},
		Body:            "{ 'test': 'data'}",
		Method:          "POST",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "POST",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/mustbepostmethod"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"http_response_code": http.StatusMethodNotAllowed,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags = map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "405",
		"result":      "success",
	}
	absentFields = []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	//check that lowercase methods work correctly
	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/mustbepostmethod"},
		Body:            "{ 'test': 'data'}",
		Method:          "head",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"http_response_code": http.StatusMethodNotAllowed,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags = map[string]interface{}{
		"server":      nil,
		"method":      "head",
		"status_code": "405",
		"result":      "success",
	}
	absentFields = []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestBody(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/musthaveabody"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/musthaveabody"},
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"http_response_code": http.StatusBadRequest,
		"result_type":        "success",
		"result_code":        0,
	}
	expectedTags = map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "400",
		"result":      "success",
	}
	absentFields = []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestStringMatch(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                 testutil.Logger{},
		URLs:                []string{ts.URL + "/good"},
		Body:                "{ 'test': 'data'}",
		Method:              "GET",
		ResponseStringMatch: "hit the good page",
		ResponseTimeout:     config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":    http.StatusOK,
		"response_string_match": 1,
		"result_type":           "success",
		"result_code":           0,
		"response_time":         nil,
		"content_length":        nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestStringMatchJson(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                 testutil.Logger{},
		URLs:                []string{ts.URL + "/jsonresponse"},
		Body:                "{ 'test': 'data'}",
		Method:              "GET",
		ResponseStringMatch: "\"service_status\": \"up\"",
		ResponseTimeout:     config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":    http.StatusOK,
		"response_string_match": 1,
		"result_type":           "success",
		"result_code":           0,
		"response_time":         nil,
		"content_length":        nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestStringMatchFail(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                 testutil.Logger{},
		URLs:                []string{ts.URL + "/good"},
		Body:                "{ 'test': 'data'}",
		Method:              "GET",
		ResponseStringMatch: "hit the bad page",
		ResponseTimeout:     config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":    http.StatusOK,
		"response_string_match": 0,
		"result_type":           "response_string_mismatch",
		"result_code":           1,
		"response_time":         nil,
		"content_length":        nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "response_string_mismatch",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test with sleep in short mode.")
	}

	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/twosecondnap"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"result_type": "timeout",
		"result_code": 4,
	}
	expectedTags := map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "timeout",
	}
	absentFields := []string{"http_response_code", "response_time", "content_length", "response_string_match"}
	absentTags := []string{"status_code"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, absentTags)
}

func TestBadRegex(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                 testutil.Logger{},
		URLs:                []string{ts.URL + "/good"},
		Body:                "{ 'test': 'data'}",
		Method:              "GET",
		ResponseStringMatch: "bad regex:[[",
		ResponseTimeout:     config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	require.ErrorContains(t, h.Init(), "failed to compile regular expression")
}

type fakeClient struct {
	statusCode int
	err        error
}

func (f *fakeClient) Do(_ *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.statusCode}, f.err
}

func TestNetworkErrors(t *testing.T) {
	cl := client{
		httpClient: &fakeClient{err: &url.Error{Err: &net.OpError{Err: &net.DNSError{Err: "DNS error"}}}},
		address:    "",
	}
	// DNS error
	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{"https://nonexistent.nonexistent"}, // Any non-resolvable URL works here
		Body:            "",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		FollowRedirects: false,
		clients:         []client{cl},
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"result_type": "dns_error",
		"result_code": 5,
	}
	expectedTags := map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "dns_error",
	}
	absentFields := []string{"http_response_code", "response_time", "content_length", "response_string_match"}
	absentTags := []string{"status_code"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, absentTags)

	// Connection failed
	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{"https:/nonexistent.nonexistent"}, // Any non-routable IP works here
		Body:            "",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 5),
		FollowRedirects: false,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"result_type": "connection_failed",
		"result_code": 3,
	}
	expectedTags = map[string]interface{}{
		"server": nil,
		"method": "GET",
		"result": "connection_failed",
	}
	absentFields = []string{"http_response_code", "response_time", "content_length", "response_string_match"}
	absentTags = []string{"status_code"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, absentTags)
}

func TestContentLength(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     len([]byte("hit the good page!")),
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)

	h = &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/musthaveabody"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		FollowRedirects: true,
	}

	acc = testutil.Accumulator{}
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields = map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     len([]byte("sent a body!")),
	}
	expectedTags = map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields = []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestRedirect(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Location", "http://example.org")
		w.WriteHeader(http.StatusMovedPermanently)
		_, err := w.Write([]byte("test"))
		require.NoError(t, err)
	})

	h := &HTTPResponse{
		URLs:                []string{ts.URL},
		ResponseStringMatch: "test",
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expected := []telegraf.Metric{
		testutil.MustMetric(
			"http_response",
			map[string]string{
				"server":      ts.URL,
				"method":      "GET",
				"result":      "success",
				"status_code": "301",
			},
			map[string]interface{}{
				"result_code":           0,
				"result_type":           "success",
				"http_response_code":    301,
				"response_string_match": 1,
				"content_length":        4,
			},
			time.Unix(0, 0),
		),
	}

	actual := acc.GetTelegrafMetrics()
	for _, m := range actual {
		m.RemoveField("response_time")
	}

	testutil.RequireMetricsEqual(t, expected, actual, testutil.IgnoreTime())
}

func TestBasicAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHeader := r.Header.Get("Authorization")
		require.Equal(t, "Basic bWU6bXlwYXNzd29yZA==", aHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Body:            "{ 'test': 'data'}",
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		Username:        config.NewSecret([]byte("me")),
		Password:        config.NewSecret([]byte("mypassword")),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func TestStatusCodeMatchFail(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                testutil.Logger{},
		URLs:               []string{ts.URL + "/nocontent"},
		ResponseStatusCode: http.StatusOK,
		ResponseTimeout:    config.Duration(time.Second * 20),
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":         http.StatusNoContent,
		"response_status_code_match": 0,
		"result_type":                "response_status_code_mismatch",
		"result_code":                6,
		"response_time":              nil,
		"content_length":             nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      http.MethodGet,
		"status_code": "204",
		"result":      "response_status_code_mismatch",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestStatusCodeMatch(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                testutil.Logger{},
		URLs:               []string{ts.URL + "/nocontent"},
		ResponseStatusCode: http.StatusNoContent,
		ResponseTimeout:    config.Duration(time.Second * 20),
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":         http.StatusNoContent,
		"response_status_code_match": 1,
		"result_type":                "success",
		"result_code":                0,
		"response_time":              nil,
		"content_length":             nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      http.MethodGet,
		"status_code": "204",
		"result":      "success",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestStatusCodeAndStringMatch(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                 testutil.Logger{},
		URLs:                []string{ts.URL + "/good"},
		ResponseStatusCode:  http.StatusOK,
		ResponseStringMatch: "hit the good page",
		ResponseTimeout:     config.Duration(time.Second * 20),
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":         http.StatusOK,
		"response_status_code_match": 1,
		"response_string_match":      1,
		"result_type":                "success",
		"result_code":                0,
		"response_time":              nil,
		"content_length":             nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      http.MethodGet,
		"status_code": "200",
		"result":      "success",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestStatusCodeAndStringMatchFail(t *testing.T) {
	mux := setUpTestMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	h := &HTTPResponse{
		Log:                 testutil.Logger{},
		URLs:                []string{ts.URL + "/nocontent"},
		ResponseStatusCode:  http.StatusOK,
		ResponseStringMatch: "hit the good page",
		ResponseTimeout:     config.Duration(time.Second * 20),
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code":         http.StatusNoContent,
		"response_status_code_match": 0,
		"response_string_match":      0,
		"result_type":                "response_status_code_mismatch",
		"result_code":                6,
		"response_time":              nil,
		"content_length":             nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      http.MethodGet,
		"status_code": "204",
		"result":      "response_status_code_mismatch",
	}
	checkOutput(t, &acc, expectedFields, expectedTags, nil, nil)
}

func TestSNI(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "super-special-hostname.example.com", r.TLS.ServerName)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	h := &HTTPResponse{
		Log:             testutil.Logger{},
		URLs:            []string{ts.URL + "/good"},
		Method:          "GET",
		ResponseTimeout: config.Duration(time.Second * 20),
		ClientConfig: tls.ClientConfig{
			InsecureSkipVerify: true,
			ServerName:         "super-special-hostname.example.com",
		},
	}

	var acc testutil.Accumulator
	require.NoError(t, h.Init())
	require.NoError(t, h.Gather(&acc))

	expectedFields := map[string]interface{}{
		"http_response_code": http.StatusOK,
		"result_type":        "success",
		"result_code":        0,
		"response_time":      nil,
		"content_length":     nil,
	}
	expectedTags := map[string]interface{}{
		"server":      nil,
		"method":      "GET",
		"status_code": "200",
		"result":      "success",
	}
	absentFields := []string{"response_string_match"}
	checkOutput(t, &acc, expectedFields, expectedTags, absentFields, nil)
}

func Test_isURLInIPv6(t *testing.T) {
	tests := []struct {
		address url.URL
		want    bool
	}{
		{
			address: parseURL(t, "http://[2001:db8:a0b:12f0::1]/index.html"),
			want:    true,
		}, {
			address: parseURL(t, "http://[2001:db8:a0b:12f0::1]:80/index.html"),
			want:    true,
		}, {
			address: parseURL(t, "https://[2001:db8:a0b:12f0::1%25eth0]:15000/"), // `%25` escapes `%`
			want:    true,
		}, {
			address: parseURL(t, "https://2001:0db8:0001:0000:0000:0ab9:C0A8:0102"),
			want:    true,
		}, {
			address: parseURL(t, "http://[2607:f8b0:4005:802::1007]/"),
			want:    true,
		}, {
			address: parseURL(t, "https://127.0.0.1"),
			want:    false,
		}, {
			address: parseURL(t, "https://google.com"),
			want:    false,
		}, {
			address: parseURL(t, "https://thispagemayexist.ornot/index.html"),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.address.String(), func(t *testing.T) {
			if got, _ := isURLInIPv6(tt.address); got != tt.want {
				t.Errorf("isURLInIPv6() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_isIPNetInIPv6(t *testing.T) {
	tests := []struct {
		address *net.IPNet
		want    bool
	}{
		{
			address: &net.IPNet{
				IP:   net.IPv4(127, 0, 0, 1),
				Mask: net.CIDRMask(8, 32),
			},
			want: false,
		}, {
			address: &net.IPNet{
				IP:   net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
				Mask: net.CIDRMask(128, 128),
			},
			want: true,
		}, {
			address: &net.IPNet{
				IP:   net.IPv4(192, 168, 0, 1),
				Mask: net.CIDRMask(24, 32),
			},
			want: false,
		}, {
			address: &net.IPNet{
				IP:   net.ParseIP("fe80::43ac:7835:471a:faba"),
				Mask: net.CIDRMask(64, 128),
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.address.String(), func(t *testing.T) {
			if got := isIPNetInIPv6(tt.address); got != tt.want {
				t.Errorf("isIPNetInIPv6() = %v, want %v", got, tt.want)
			}
		})
	}
}

func parseURL(t *testing.T, address string) url.URL {
	u, err := url.Parse(address)
	require.NoError(t, err)
	require.NotNil(t, u)
	return *u
}
