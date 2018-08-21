package transport_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/apm-agent-go/model"
	"github.com/elastic/apm-agent-go/transport"
)

func init() {
	// Don't let the environment influence tests.
	os.Setenv("ELASTIC_APM_SERVER_TIMEOUT", "")
	os.Setenv("ELASTIC_APM_SERVER_URL", "")
	os.Setenv("ELASTIC_APM_SECRET_TOKEN", "")
	os.Setenv("ELASTIC_APM_VERIFY_SERVER_CERT", "")
}

func TestNewHTTPTransportDefaultURL(t *testing.T) {
	var h recordingHandler
	server := httptest.NewUnstartedServer(&h)
	defer server.Close()

	lis, err := net.Listen("tcp", "localhost:8200")
	if err != nil {
		t.Skipf("cannot listen on default server address: %s", err)
	}
	server.Listener.Close()
	server.Listener = lis
	server.Start()

	transport, err := transport.NewHTTPTransport("", "")
	assert.NoError(t, err)
	err = transport.SendStream(context.Background(), emptyStream())
	assert.NoError(t, err)
	assert.Len(t, h.requests, 1)
}

func TestHTTPTransportUserAgent(t *testing.T) {
	var h recordingHandler
	server := httptest.NewServer(&h)
	defer server.Close()
	defer patchEnv("ELASTIC_APM_SERVER_URL", server.URL)()

	transport, err := transport.NewHTTPTransport("", "")
	assert.NoError(t, err)
	err = transport.SendStream(context.Background(), emptyStream())
	assert.NoError(t, err)
	assert.Len(t, h.requests, 1)

	transport.SetUserAgent("foo")
	err = transport.SendStream(context.Background(), emptyStream())
	assert.NoError(t, err)
	assert.Len(t, h.requests, 2)

	assert.Regexp(t, "Go-http-client/.*", h.requests[0].UserAgent())
	assert.Equal(t, "foo", h.requests[1].UserAgent())
}

func TestHTTPTransportSecretToken(t *testing.T) {
	var h recordingHandler
	server := httptest.NewServer(&h)
	defer server.Close()

	transport, err := transport.NewHTTPTransport(server.URL, "hunter2")
	assert.NoError(t, err)
	transport.SendStream(context.Background(), emptyStream())

	assert.Len(t, h.requests, 1)
	assertAuthorization(t, h.requests[0], "hunter2")
}

func TestHTTPTransportEnvSecretToken(t *testing.T) {
	var h recordingHandler
	server := httptest.NewServer(&h)
	defer server.Close()
	defer patchEnv("ELASTIC_APM_SECRET_TOKEN", "hunter2")()

	transport, err := transport.NewHTTPTransport(server.URL, "")
	assert.NoError(t, err)
	transport.SendStream(context.Background(), emptyStream())

	assert.Len(t, h.requests, 1)
	assertAuthorization(t, h.requests[0], "hunter2")
}

func TestHTTPTransportNoSecretToken(t *testing.T) {
	var h recordingHandler
	transport, server := newHTTPTransport(t, &h)
	defer server.Close()

	transport.SendStream(context.Background(), emptyStream())

	assert.Len(t, h.requests, 1)
	assertAuthorization(t, h.requests[0], "")
}

func TestHTTPTransportTLS(t *testing.T) {
	var h recordingHandler
	server := httptest.NewUnstartedServer(&h)
	server.Config.ErrorLog = log.New(ioutil.Discard, "", 0)
	server.StartTLS()
	defer server.Close()

	transport, err := transport.NewHTTPTransport(server.URL, "")
	assert.NoError(t, err)

	p := emptyStream()

	// Send should fail, because we haven't told the client
	// about the CA certificate, nor configured it to disable
	// certificate verification.
	err = transport.SendStream(context.Background(), p)
	assert.Error(t, err)

	// Reconfigure the transport so that it knows about the
	// CA certificate. We avoid using server.Client here, as
	// it is not available in older versions of Go.
	certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	assert.NoError(t, err)
	certpool := x509.NewCertPool()
	certpool.AddCert(certificate)
	transport.Client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: certpool,
		},
	}
	err = transport.SendStream(context.Background(), p)
	assert.NoError(t, err)
}

func TestHTTPTransportEnvVerifyServerCert(t *testing.T) {
	var h recordingHandler
	server := httptest.NewTLSServer(&h)
	defer server.Close()

	defer patchEnv("ELASTIC_APM_VERIFY_SERVER_CERT", "false")()

	transport, err := transport.NewHTTPTransport(server.URL, "")
	assert.NoError(t, err)

	assert.NotNil(t, transport.Client)
	assert.IsType(t, &http.Transport{}, transport.Client.Transport)
	httpTransport := transport.Client.Transport.(*http.Transport)
	assert.NotNil(t, httpTransport.TLSClientConfig)
	assert.True(t, httpTransport.TLSClientConfig.InsecureSkipVerify)

	err = transport.SendStream(context.Background(), emptyStream())
	assert.NoError(t, err)
}

func TestHTTPError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "error-message", http.StatusInternalServerError)
	})
	tr, server := newHTTPTransport(t, h)
	defer server.Close()

	err := tr.SendStream(context.Background(), emptyStream())
	assert.EqualError(t, err, "request failed with 500 Internal Server Error: error-message")
}

func TestHTTPTransportContent(t *testing.T) {
	var h recordingHandler
	server := httptest.NewServer(&h)
	defer server.Close()

	stream := transport.NewStream()
	go func() {
		for i := 0; i < 1024; i++ {
			stream.WriteTransaction(model.Transaction{})
		}
		stream.Close()
	}()

	transport, err := transport.NewHTTPTransport(server.URL, "")
	assert.NoError(t, err)
	transport.SendStream(context.Background(), stream)

	require.Len(t, h.requests, 1)
	assert.Equal(t, "deflate", h.requests[0].Header.Get("Content-Encoding"))
	assert.Equal(t, "application/x-ndjson", h.requests[0].Header.Get("Content-Type"))
}

func TestHTTPTransportServerTimeout(t *testing.T) {
	done := make(chan struct{})
	blockingHandler := func(w http.ResponseWriter, req *http.Request) { <-done }
	server := httptest.NewServer(http.HandlerFunc(blockingHandler))
	defer server.Close()
	defer close(done)
	defer patchEnv("ELASTIC_APM_SERVER_TIMEOUT", "50ms")()

	before := time.Now()
	transport, err := transport.NewHTTPTransport(server.URL, "")
	assert.NoError(t, err)
	err = transport.SendStream(context.Background(), emptyStream())
	taken := time.Since(before)
	assert.Error(t, err)
	err = errors.Cause(err)
	assert.Implements(t, new(net.Error), err)
	assert.True(t, err.(net.Error).Timeout())
	assert.Condition(t, func() bool {
		return taken >= 50*time.Millisecond
	})
}

func newHTTPTransport(t *testing.T, handler http.Handler) (*transport.HTTPTransport, *httptest.Server) {
	server := httptest.NewServer(handler)
	transport, err := transport.NewHTTPTransport(server.URL, "")
	if !assert.NoError(t, err) {
		server.Close()
		t.FailNow()
	}
	return transport, server
}

func emptyStream() *transport.Stream {
	stream := transport.NewStream()
	go stream.Close()
	return stream
}
