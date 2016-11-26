package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"

	"github.com/ericchiang/k8s/api/unversioned"
	"github.com/ericchiang/k8s/api/v1"
	"github.com/ericchiang/k8s/runtime"
)

const contentTypePB = "application/vnd.kubernetes.protobuf"

// https://github.com/kubernetes/kubernetes/blob/master/docs/proposals/protobuf.md#wire-format
var magicBytes = []byte{0x6b, 0x38, 0x73, 0x00}

func unmarshal(b []byte, obj proto.Unmarshaler) error {
	if len(b) < len(magicBytes) {
		return errors.New("magic bytes not present")
	}
	if !bytes.Equal(b[:len(magicBytes)], magicBytes) {
		return errors.New("magic bytes not present")
	}
	u := new(runtime.Unknown)
	if err := u.Unmarshal(b[len(magicBytes):]); err != nil {
		return fmt.Errorf("unmarshal unknown: %v", err)
	}
	return obj.Unmarshal(u.Raw)
}

func marshal(obj proto.Marshaler) ([]byte, error) {
	payload, err := obj.Marshal()
	if err != nil {
		return nil, err
	}
	body, err := (&runtime.Unknown{Raw: payload}).Marshal()
	if err != nil {
		return nil, err
	}
	d := make([]byte, len(magicBytes)+len(body))
	copy(d[:len(magicBytes)], magicBytes)
	copy(d[len(magicBytes):], body)
	return d, nil
}

// Client is a Kuberntes client.
type Client struct {
	// The URL of the API server.
	Endpoint string

	// Default namespaces for objects that don't supply a namespace in
	// their object metadata. If empty the "default" namespace is used.
	Namespace string

	Client *http.Client
}

// InCluster returns a client that uses the service account bearer token mounted
// into Kubernetes pods.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		return nil, errors.New("unable to load in-cluster configuration, KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT must be defined")
	}

	caData, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(caData) {
		return nil, errors.New("service account certiifcate file doesn't contain any certificates")
	}

	token, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, err
	}
	namespace, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return nil, err
	}
	return &Client{
		Endpoint:  "https://" + host + ":" + port,
		Namespace: string(namespace),
		Client: &http.Client{
			Transport: &bearerTokenTransport{
				token: string(token),
				base: &http.Transport{
					Proxy: http.ProxyFromEnvironment,
					DialContext: (&net.Dialer{
						Timeout:   30 * time.Second,
						KeepAlive: 30 * time.Second,
					}).DialContext,
					TLSClientConfig:       &tls.Config{RootCAs: rootCAs},
					MaxIdleConns:          100,
					IdleConnTimeout:       90 * time.Second,
					TLSHandshakeTimeout:   10 * time.Second,
					ExpectContinueTimeout: 1 * time.Second,
				},
			},
		},
	}, nil
}

type bearerTokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTokenTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	// Per http.RoundTripper contract don't modify the underlying request.
	r := new(http.Request)
	*r = *req
	r.Header = make(http.Header, len(req.Header)+1)
	for k, s := range req.Header {
		r.Header[k] = append([]string(nil), s...)
	}
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// Error is an error from a unexpected status code.
type Error struct {
	// The status object returned by the Kubernetes API,
	Status *unversioned.Status
}

func (e *Error) Error() string { return e.Status.Message }

func checkStatusCode(statusCode, gotStatusCode int, body []byte) error {
	if statusCode == gotStatusCode {
		return nil
	}

	status := new(unversioned.Status)
	if err := unmarshal(body, status); err != nil {
		return fmt.Errorf("decode error status: %v", err)
	}
	return &Error{status}
}

type object interface {
	GetMetadata() *v1.ObjectMeta
}

func (c *Client) client() *http.Client {
	if c.Client == nil {
		return http.DefaultClient
	}
	return c.Client
}

func (c *Client) namespaceFor(ns string, namespaced bool) string {
	if !namespaced {
		return ""
	}
	if ns != "" {
		return ns
	}
	if c.Namespace != "" {
		return c.Namespace
	}
	return "default"
}

func (c *Client) urlFor(apiGroup, apiVersion, namespace, resource, name string) string {
	basePath := "apis/"
	if apiGroup == "" {
		basePath = "api/"
	}

	var p string
	if namespace != "" {
		p = path.Join(basePath, apiGroup, apiVersion, "namespaces", namespace, resource, name)
	} else {
		p = path.Join(basePath, apiGroup, apiVersion, resource, name)
	}
	if strings.HasSuffix(c.Endpoint, "/") {
		return c.Endpoint + p
	}
	return c.Endpoint + "/" + p
}

func (c *Client) create(ctx context.Context, url string, req proto.Marshaler, resp proto.Unmarshaler) error {
	body, err := marshal(req)
	if err != nil {
		return err
	}

	r, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", contentTypePB)
	r.Header.Set("Accept", contentTypePB)

	re, err := c.client().Do(r)
	if err != nil {
		return err
	}
	defer re.Body.Close()

	respBody, err := ioutil.ReadAll(re.Body)
	if err != nil {
		return fmt.Errorf("read body: %v", err)
	}

	if err := checkStatusCode(re.StatusCode, http.StatusCreated, respBody); err != nil {
		return err
	}
	return unmarshal(respBody, resp)
}

func (c *Client) delete(ctx context.Context, url string, name string) error {
	r, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	r.Header.Set("Accept", contentTypePB)
	re, err := c.client().Do(r)
	if err != nil {
		return err
	}
	defer re.Body.Close()

	respBody, err := ioutil.ReadAll(re.Body)
	if err != nil {
		return fmt.Errorf("read body: %v", err)
	}

	if err := checkStatusCode(re.StatusCode, http.StatusOK, respBody); err != nil {
		return err
	}
	return nil
}

func (c *Client) get(ctx context.Context, url string, resp proto.Unmarshaler) error {
	r, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	r.Header.Set("Accept", contentTypePB)
	re, err := c.client().Do(r)
	if err != nil {
		return err
	}
	defer re.Body.Close()

	respBody, err := ioutil.ReadAll(re.Body)
	if err != nil {
		return fmt.Errorf("read body: %v", err)
	}

	if err := checkStatusCode(re.StatusCode, http.StatusOK, respBody); err != nil {
		return err
	}
	return unmarshal(respBody, resp)
}
