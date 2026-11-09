package service

import (
	"errors"
	"fmt"
	"strings"
)

func sanitizeOpenAIRejectedEncryptedContent(body []byte) ([]byte, bool, error) {
	var decoded map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &decoded); err != nil {
		return body, false, fmt.Errorf("decode encrypted-content recovery: %w", err)
	}
	if !trimOpenAIEncryptedReasoningItems(decoded) {
		return body, false, nil
	}
	cleaned, err := marshalOpenAIUpstreamJSON(decoded)
	return cleaned, err == nil, err
}

// This is a rejected request-history error, not a measure of account health.
type openAIInvalidEncryptedContentError struct{ cause error }

func (e *openAIInvalidEncryptedContentError) Error() string { return e.cause.Error() }
func (e *openAIInvalidEncryptedContentError) Unwrap() error { return e.cause }

func openAIInvalidEncryptedContentSignal(code, message string) bool {
	if strings.EqualFold(strings.TrimSpace(code), "invalid_encrypted_content") {
		return true
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "invalid_encrypted_content") ||
		(strings.Contains(message, "encrypted content") &&
			(strings.Contains(message, "hydration failed") || strings.Contains(message, "could not be verified")))
}

func isOpenAIInvalidEncryptedContentResponse(payload []byte, message string) bool {
	return openAIInvalidEncryptedContentSignal(extractUpstreamErrorCode(payload), message) ||
		openAIInvalidEncryptedContentSignal(openAIStreamFailedEventErrorCode(payload), message)
}

func isOpenAIInvalidEncryptedContentError(err error) bool {
	if err == nil {
		return false
	}
	var rejected *openAIInvalidEncryptedContentError
	return errors.As(err, &rejected) || openAIInvalidEncryptedContentSignal("", err.Error())
}

func wrapOpenAIInvalidEncryptedContentError(err error, rejected bool) error {
	if err == nil || !rejected {
		return err
	}
	return &openAIInvalidEncryptedContentError{cause: err}
}

func openAIEncryptedInputItemKind(kind, role string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		switch strings.TrimSpace(role) {
		case "user", "assistant", "system", "developer":
			return "message"
		}
	}
	return kind
}
