package service

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// OpenAI synchronous requests stop when their HTTP stream is canceled. Keep
// the caller's deadline/cancellation through upload, headers and response reads;
// other providers retain their existing detached usage-drain policy.
func openAIAccountUpstreamContext(ctx context.Context, account *Account) (context.Context, context.CancelFunc) {
	if account == nil || account.Platform != PlatformOpenAI {
		return detachUpstreamContext(ctx)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, func() {}
}

// Closing a response must cancel before closing the body: some transports wait
// for their request context while closing. Canceling one HTTP/2 stream leaves
// the shared connection available to other requests.
type openAICancelReadCloser struct {
	io.ReadCloser
	ctx        context.Context
	cancel     context.CancelFunc
	stopCancel func() bool
	once       sync.Once
	err        error
}

func (b *openAICancelReadCloser) Close() error {
	// Ordinary response cleanup need not schedule the disconnect callback.
	if b.stopCancel != nil {
		b.stopCancel()
	}
	return b.close()
}

func (b *openAICancelReadCloser) close() error {
	b.once.Do(func() {
		b.cancel()
		b.err = b.ReadCloser.Close()
	})
	return b.err
}

func (b *openAICancelReadCloser) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.ctx.Err() != nil {
		return n, b.ctx.Err()
	}
	return n, err
}

func closeOpenAIUpstreamOnDisconnect(resp *http.Response, account *Account) {
	if account != nil && account.Platform == PlatformOpenAI && resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// Check the incoming context, not the completed attempt's child context: closing
// a fully read error response also cancels that child as ordinary cleanup.
func openAIAccountContextError(ctx context.Context, account *Account) error {
	if ctx != nil && account != nil && account.Platform == PlatformOpenAI {
		return ctx.Err()
	}
	return nil
}
