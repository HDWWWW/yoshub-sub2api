package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Native WS clients can use the HTTP transport for large turns. That is still
// one dispatch: neither an HTTP failure nor SSE visibility proves safe replay.
func (s *OpenAIGatewayService) proxySmartWSHTTPBridge(
	ctx context.Context, c *gin.Context, account *Account, request *http.Request, proxyURL string, body []byte,
	model, upstreamModel, imageBillingModel, imageSize, imageInputSize string,
	toolMapping apicompat.ResponsesClientToolMapping, write func([]byte) error,
) (result *OpenAIForwardResult, returnErr error) {
	started := time.Now()
	dispatch := beginSmartWSDispatch(c)
	observation := s.beginSmartWSObservation(nil)
	defer func() {
		returnErr = stopSmartWSReplay(c, returnErr)
		observation.finish(ctx, result, returnErr)
	}()
	// The underlying HTTP transport may fail after writing request bytes.
	// Conservatively cross the boundary before entering RoundTrip.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dispatch.state.Store(smartWSMayBeSent)
	observation.dispatched = true
	request = request.WithContext(WithHTTPUpstreamRedirectsDisabled(request.Context()))
	request.GetBody = nil
	resp, err := s.doOpenAIUpstream(request, proxyURL, account)
	if err != nil {
		return nil, smartWSUncertainError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	upstreamBody := resp.Body
	stopCancel := context.AfterFunc(ctx, func() { _ = upstreamBody.Close() })
	defer stopCancel()
	dispatch.state.Store(smartWSReceived)
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, openAIWSHTTPBridgeErrorBodyLimitBytes))
		payload = s.redactAgentIdentitySensitiveBody(ctx, account, payload)
		dispatch.state.Store(smartWSTerminal)
		observation.event(smartWSNativeErrorPayload(resp.StatusCode, payload, resp.Header))
		result = smartWSHTTPFailureResult(resp, payload, body, model, upstreamModel, true, started)
		if result != nil {
			result.OpenAIWSMode = true
		}
		if !markOpenAICyberPolicyEvent(c, payload, resp.StatusCode, nil) {
			_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, payload, upstreamModel)
		}
		return result, writeSmartWSNativeFailure(c, resp.StatusCode, payload, resp.Header, write)
	}

	result = &OpenAIForwardResult{Model: model, UpstreamModel: upstreamModel, Stream: true, OpenAIWSMode: true,
		ResponseHeaders: cloneHeader(resp.Header), ServiceTier: extractOpenAIServiceTierFromBody(body),
		ReasoningEffort:          extractOpenAIReasoningEffortFromBody(body, upstreamModel, model),
		RequestedReasoningEffort: CanonicalRequestedReasoningEffort(body, model, upstreamModel)}
	modelObserver := s.newAccountRiskModelObserver(ctx, c, account, upstreamModel)
	imageCounter := newOpenAIImageOutputCounter()
	replayCollector := &openAIWSToolCallReplayCollector{}
	defer func() {
		result.Duration = time.Since(started)
		result.UpstreamResponseModel, result.UpstreamResponseModelConflict = modelObserver.Model(), modelObserver.Conflict()
		result.UpstreamResponseServiceTier = modelObserver.ServiceTier()
		result.ServiceTier = resolvedOpenAIUpstreamServiceTierFromObserver(modelObserver, result.ServiceTier)
		result.wsReplayInput = replayCollector.Items()
		result.wsReplayInputExists = len(result.wsReplayInput) > 0
		result.wsAccountFailoverReplayInput = replayCollector.AllItems()
		if result.ImageCount = imageCounter.Count(); result.ImageCount > 0 {
			result.ImageSize, result.ImageInputSize, result.BillingModel = imageSize, imageInputSize, imageBillingModel
			result.ImageOutputSizes = imageCounter.Sizes()
		}
		mergeSmartWSCyberPolicyUsage(c, &result.Usage)
	}()
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	if limit := s.getOpenAIWSConnPool().upstreamFrameLimit(); limit > 0 && int64(maxLineSize) > limit {
		maxLineSize = int(limit)
	}
	if hasResponsesClientToolMapping(toolMapping) {
		resp.Body = newResponsesClientToolStreamBody(resp.Body, toolMapping, maxLineSize, s.cfg)
	}
	scanner := bufio.NewScanner(resp.Body)
	buffer := getSSEScannerBuf64K()
	scanner.Buffer(buffer[:0], maxLineSize)
	scanner.Split(newBoundedSSESplit(s.cfg, maxLineSize, nil))
	defer putSSEScannerBuf64K(buffer)
	eventName := ""
	var createdAtStamper responsesCreatedAtStamper
	var firstFailure []byte
	failureStatus, tailFrames := 0, 0
	var tailTimer *time.Timer
	defer func() {
		if tailTimer != nil {
			tailTimer.Stop()
		}
	}()
	for scanner.Scan() {
		line := scanner.Text()
		if typ, ok := extractOpenAISSEEventLine(line); ok {
			eventName = typ
			continue
		}
		if strings.TrimSpace(line) == "" {
			eventName = ""
			continue
		}
		data, ok := extractOpenAISSEDataLine(line)
		if !ok || strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		payload := []byte(openAICompatPayloadWithEventType(data, eventName))
		if normalized, changed := normalizeCompletedImageGenerationStatus(payload); changed {
			payload = normalized
		}
		if !gjson.ValidBytes(payload) {
			return result, errors.New("invalid upstream http bridge event")
		}
		typ, id, _ := parseOpenAIWSEventEnvelope(payload)
		if len(firstFailure) > 0 {
			// A bare error is already the user's outcome. Only its matching
			// authoritative failed tail may enrich billing; never hide it behind
			// a later completed event or consume a different turn's result.
			tailFrames++
			if (id != "" && result.RequestID != "" && id != result.RequestID) || typ == "response.created" ||
				(isOpenAIWSTerminalEvent(typ) && typ != "response.failed") || tailFrames > 8 {
				break
			}
			if typ != "response.failed" {
				continue
			}
		}
		if id != "" {
			result.RequestID, result.ResponseID = id, id
		}
		modelObserver.ObserveOpenAI(payload, typ)
		observation.event(payload)
		if openAIWSMessageShouldParseUsage(typ, payload) {
			parseOpenAIWSResponseUsageFromCompletedEvent(payload, &result.Usage)
		}
		imageCounter.AddSSEData(payload)
		replayCollector.AddEvent(typ, payload)
		if result.FirstTokenMs == nil && smartWSUsefulOutput(payload, typ) {
			ms := int(time.Since(started).Milliseconds())
			result.FirstTokenMs = &ms
		}
		terminal := isOpenAIWSTerminalEvent(typ)
		failed := typ == "error" || typ == "response.failed"
		if terminal || failed {
			dispatch.state.Store(smartWSTerminal)
			result.UpstreamTerminalEvent = typ
		}
		if failed {
			markOpenAICyberPolicyEvent(c, payload, http.StatusOK, &result.Usage)
			if len(firstFailure) == 0 {
				firstFailure = append([]byte(nil), payload...)
				failureStatus = openAIHTTPWSEventErrorStatus(payload)
				s.handleSmartWSFailureSideEffects(ctx, c, account, upstreamModel, resp.Header, payload)
			}
		}
		clientPayload := payload
		if !failed && model != upstreamModel && openAIWSEventMayContainModel(typ) {
			clientPayload = replaceOpenAIWSMessageModel(payload, upstreamModel, model)
		}
		if !failed {
			if stamped, changed := createdAtStamper.Stamp(clientPayload, typ); changed {
				clientPayload = stamped
			}
		}
		if err := write(clientPayload); err != nil {
			result.ClientDisconnect = true
			return result, fmt.Errorf("write client websocket event: %w", err)
		}
		if failed {
			c.Set(smartWSFailureDeliveredKey, true)
			markOpenAIWSClientVisibleFailure(c, typ, payload)
		}
		if terminal {
			if len(firstFailure) > 0 {
				return result, smartWSStopFailure(failureStatus, firstFailure, resp.Header)
			}
			return result, nil
		}
		if typ == "error" && tailTimer == nil {
			tailTimer = time.AfterFunc(200*time.Millisecond, func() { _ = resp.Body.Close() })
		}
	}
	if len(firstFailure) > 0 {
		return result, smartWSStopFailure(failureStatus, firstFailure, resp.Header)
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read upstream http bridge stream: %w", err)
	}
	return result, errors.New("upstream http bridge stream ended before terminal event")
}
