package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/starcat-app/starcat-history-api/internal/serving"
)

// PublishHandler 接收本地数据平台生成的快照和日增量包。
type PublishHandler struct {
	registry     *serving.Registry
	maximumBytes int64
}

func NewPublishHandler(registry *serving.Registry, maximumBytes int64) *PublishHandler {
	return &PublishHandler{registry: registry, maximumBytes: maximumBytes}
}

// HandleSnapshotUpload 校验、安装并按 activate 参数切换完整快照。
func (h *PublishHandler) HandleSnapshotUpload(w http.ResponseWriter, r *http.Request) {
	if !requireZip(w, r) {
		return
	}
	activate, err := strconv.ParseBool(r.URL.Query().Get("activate"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "activate must be true or false", nil)
		return
	}
	if err := h.registry.EnsureInstallCapacity(r.ContentLength); err != nil {
		writePublishError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maximumBytes)
	manifest, err := h.registry.InstallSnapshotZip(r.Context(), r.PathValue("model_version"), r.Body, activate, h.maximumBytes)
	if err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_version": manifest.ModelVersion, "active_watermark": manifest.SourceWatermark,
		"repositories": manifest.Repositories, "active": activate,
	})
}

// HandleSnapshotActivate 在无需重新上传大文件的情况下回滚到已安装版本。
func (h *PublishHandler) HandleSnapshotActivate(w http.ResponseWriter, r *http.Request) {
	if err := h.registry.ActivateSnapshot(r.PathValue("model_version")); err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model_version": r.PathValue("model_version"), "active": true})
}

// HandleDeltaUpload 校验并幂等应用日增量。
func (h *PublishHandler) HandleDeltaUpload(w http.ResponseWriter, r *http.Request) {
	if !requireZip(w, r) {
		return
	}
	if err := h.registry.EnsureInstallCapacity(r.ContentLength); err != nil {
		writePublishError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maximumBytes)
	manifest, applied, err := h.registry.InstallDeltaZip(r.Context(), r.PathValue("delta_id"), r.Body, h.maximumBytes)
	if err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"delta_id": manifest.DeltaID, "active_watermark": manifest.ToWatermark,
		"rows": manifest.Rows, "applied": applied,
	})
}

// HandleActive 返回当前 Serving 水位。
func (h *PublishHandler) HandleActive(w http.ResponseWriter, r *http.Request) {
	active, err := h.registry.Active(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "Unable to read active history version.", nil)
		return
	}
	writeJSON(w, http.StatusOK, active)
}

func requireZip(w http.ResponseWriter, r *http.Request) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/zip" {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/zip", nil)
		return false
	}
	return true
}

func writePublishError(w http.ResponseWriter, err error) {
	var maximum *http.MaxBytesError
	switch {
	case errors.As(err, &maximum):
		writeError(w, http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE", "Bundle exceeds size limit.", nil)
	case errors.Is(err, serving.ErrVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", "The immutable bundle identifier already has different content.", nil)
	case errors.Is(err, serving.ErrWatermarkConflict):
		writeError(w, http.StatusConflict, "WATERMARK_CONFLICT", err.Error(), nil)
	case errors.Is(err, serving.ErrInvalidBundle):
		writeError(w, http.StatusUnprocessableEntity, "INVALID_BUNDLE", err.Error(), nil)
	case errors.Is(err, serving.ErrInsufficientStorage):
		writeError(w, http.StatusInsufficientStorage, "INSUFFICIENT_STORAGE", err.Error(), nil)
	default:
		writeError(w, http.StatusInternalServerError, "PUBLISH_FAILED", "History bundle publish failed.", nil)
	}
}
