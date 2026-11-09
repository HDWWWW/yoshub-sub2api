package service

import (
	"context"
	"net/http"
)

type smartWSHandshakeKey struct{}

// Provenance is internal response context, never a client-controlled header.
type smartWSHandshakeFailure struct {
	category       string
	accountFailure bool
}

func (f smartWSHandshakeFailure) annotate(err *UpstreamFailoverError) {
	err.Stage = GatewayFailureStageWSHandshake
	err.Scope = GatewayFailureScopeProvider
	err.Reason = GatewayFailureReason(f.category)
	if f.accountFailure {
		err.Scope = GatewayFailureScopeAccount
	}
}

func classifySmartWSHandshake(status int, body []byte) smartWSHandshakeFailure {
	if isOpenAICyberAccessDenied(status, body) {
		return smartWSHandshakeFailure{"cyber_access_denied", false}
	}
	if hit, code, _ := detectOpenAICyberPolicy(body); hit {
		return smartWSHandshakeFailure{code, false}
	}
	switch status {
	case http.StatusUnauthorized:
		return smartWSHandshakeFailure{"http_401", true}
	case http.StatusForbidden:
		if openAIStream403AccountFailure(body, "") {
			return smartWSHandshakeFailure{"credential_rejected", true}
		}
		return smartWSHandshakeFailure{"ws_forbidden", false}
	case http.StatusTooManyRequests:
		return smartWSHandshakeFailure{"rate_limited", true}
	case 529:
		return smartWSHandshakeFailure{"upstream_overloaded", true}
	case 404, 405, 426, 501:
		return smartWSHandshakeFailure{"ws_unsupported", false}
	default:
		return smartWSHandshakeFailure{"upstream_error", false}
	}
}

func smartWSHandshakeErrorResponse(req *http.Request, dialErr *openAIWSDialError) *http.Response {
	classification := classifySmartWSHandshake(dialErr.StatusCode, dialErr.ResponseBody)
	resp := openAIHTTPWSErrorResponse(req, dialErr.StatusCode, dialErr.ResponseHeaders, dialErr.ResponseBody)
	resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), smartWSHandshakeKey{}, classification))
	return resp
}
