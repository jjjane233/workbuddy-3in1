package trae

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDetailParamUsesSoloHeaders(t *testing.T) {
	var got *http.Request
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"config_info_list":[]}`)),
			Request:    r,
		}, nil
	})}
	a := &auth.Auth{AccessToken: "TOKEN", UID: "UID", DeviceID: "DEVICE"}
	if _, err := New(a, Options{HTTPClient: hc}).DetailParam(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("request was not captured")
	}
	if got.Method != http.MethodPost || got.URL.Path != PathDetailParam {
		t.Fatalf("request=%s %s, want POST %s", got.Method, got.URL.Path, PathDetailParam)
	}
	checks := map[string]string{
		"Authorization":      "Cloud-IDE-JWT TOKEN",
		"X-Ide-Version":      SoloIDEVersion,
		"X-Ide-Version-Code": SoloIDEVersionCode,
		"X-Uid":              "UID",
	}
	for key, want := range checks {
		if got.Header.Get(key) != want {
			t.Errorf("%s=%q, want %q", key, got.Header.Get(key), want)
		}
	}
}
