package hermes

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

// maxDrainBytes bounds how much of a discarded body is read so its connection can be reused.
const maxDrainBytes = 64 << 10

// releaseBody runs release once, on Close or at EOF, so a context or slot tied to a response lives as long as its body.
type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func releaseOnClose(body io.ReadCloser, release func()) io.ReadCloser {
	return &releaseBody{ReadCloser: body, release: release}
}

func (b *releaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// nonNilBodyTransport gives every response a Body, as net/http's Client does, so the layers above can wrap and close it whatever the base transport returns.
type nonNilBodyTransport struct {
	next http.RoundTripper
}

func (t nonNilBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if resp != nil && resp.Body == nil {
		resp.Body = http.NoBody
	}
	return resp, err
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
}

// closeRequestBody honours the RoundTripper contract of closing the request body even when the request is never sent.
func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
