package router

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// Image endpoints (OpenAI images API shape, served by the sd-cpp creative
// backends). /v1/images/generations is plain JSON and reuses the generic
// handleProxy. /v1/images/edits is multipart/form-data, which handleProxy's
// JSON parse would reject — handleProxyMultipart below resolves the model
// from the form and forwards the body VERBATIM.
//
// No model rewrite happens on the multipart path: each creative backend
// serves exactly one model and ignores the field, and rewriting one part of
// a multipart stream means re-encoding the whole body for no behavioural
// gain. If a multi-model image backend ever appears, revisit.

// maxMultipartModelScan bounds how much of a form part is read while looking
// for the model field's value — a model name, not an image.
const maxMultipartModelScan = 4 * 1024

// handleProxyMultipart resolves the "model" field of a multipart/form-data
// request under the given api_class and reverse-proxies the untouched body
// upstream. Images are never role-resolved, so there is no failover loop;
// like handleProxy, every request emits one reqlog.Record on exit.
func (rt *Router) handleProxyMultipart(requireClass config.APIClass) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recordingWriter{ResponseWriter: w}
		cap := &responseCapture{}

		var (
			modelIn  string
			resolved *resolveResult
			errMsg   string
		)

		defer func() {
			lr := reqlog.Record{
				RequestID: httpx.RequestIDFromContext(r.Context()),
				TS:        start,
				Method:    r.Method,
				Path:      r.URL.Path,
				Model:     modelIn,
				Status:    rec.status,
				LatencyMS: int(time.Since(start) / time.Millisecond),
				Stream:    cap.isSSE,
				Error:     errMsg,
			}
			if resolved != nil {
				lr.BackendModel = resolved.BackendModel
				lr.BackendURL = resolved.BackendURL
				lr.ResolvedVia = resolved.ModelID
				lr.APIClass = string(resolved.APIClass)
			}
			rt.sink.Log(lr)
			rt.metrics.Observe(lr)
		}()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			errMsg = "read body: " + err.Error()
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
			errMsg = "expected multipart/form-data with a boundary"
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}

		model, err := multipartFormValue(bytes.NewReader(body), params["boundary"], "model")
		if err != nil {
			errMsg = "parse multipart form: " + err.Error()
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}
		if model == "" {
			errMsg = `missing "model" form field`
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}
		modelIn = model

		res, err := rt.resolveModel(model, true)
		if err != nil {
			errMsg = err.Error()
			rt.logger.WarnContext(r.Context(), "resolve failed", "model", model, "err", err)
			http.Error(rec, errMsg, http.StatusNotFound)
			return
		}
		resolved = &res

		if requireClass != "" && res.APIClass != requireClass {
			errMsg = fmt.Sprintf("model %q has api_class %q; %s requires %q",
				model, res.APIClass, r.URL.Path, requireClass)
			rt.logger.WarnContext(r.Context(), "api_class mismatch",
				"model", model, "got", string(res.APIClass), "want", string(requireClass), "path", r.URL.Path)
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}

		rt.logger.InfoContext(r.Context(), "forwarding",
			"path", r.URL.Path,
			"model", model, "backend_model", res.BackendModel,
			"backend_url", res.BackendURL, "resolved_via", res.ModelID)

		upstreamErr := rt.reverseProxyTo(rec, r, res.BackendURL, body, res.AuthBearer, res.AuthHeader, cap, false)
		if upstreamErr == nil {
			rt.avail.ReportSuccess(res.ModelID)
			return
		}
		rt.avail.ReportFailure(res.ModelID, upstreamErr)
		errMsg = "upstream: " + upstreamErr.Error()
		if rec.status == 0 {
			http.Error(rec, errMsg, http.StatusBadGateway)
		}
	}
}

// multipartFormValue scans the multipart stream for the named non-file field
// and returns its value. Parts are streamed, so a multi-megabyte image part
// costs an io.Copy to discard, never a buffer.
func multipartFormValue(body io.Reader, boundary, field string) (string, error) {
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if part.FormName() == field && part.FileName() == "" {
			val, err := io.ReadAll(io.LimitReader(part, maxMultipartModelScan))
			part.Close()
			if err != nil {
				return "", err
			}
			return string(val), nil
		}
		// Not the field we want — drain so the reader can advance.
		_, _ = io.Copy(io.Discard, part)
		part.Close()
	}
}
