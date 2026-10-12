package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/httpx"
)

// stubAvatarService — ручной стаб AvatarService. Поведение задаётся функциями-полями.
type stubAvatarService struct {
	uploadFn     func(ctx context.Context, userID, fileName, contentType string, data []byte) (*domain.Avatar, error)
	getByIDFn    func(ctx context.Context, id uuid.UUID) (*domain.Avatar, error)
	listFn       func(ctx context.Context, userID string) ([]*domain.Avatar, error)
	getContentFn func(ctx context.Context, id uuid.UUID, size string) (io.ReadCloser, *domain.ObjectInfo, error)
	deleteFn     func(ctx context.Context, id uuid.UUID, requesterID string) error
}

// Проверка на этапе компиляции: стаб реализует интерфейс хендлера.
var _ AvatarService = (*stubAvatarService)(nil)

func (s *stubAvatarService) Upload(ctx context.Context, userID, fileName, contentType string, data []byte) (*domain.Avatar, error) {
	if s.uploadFn != nil {
		return s.uploadFn(ctx, userID, fileName, contentType, data)
	}
	return nil, domain.ErrNotFound
}

func (s *stubAvatarService) GetByID(ctx context.Context, id uuid.UUID) (*domain.Avatar, error) {
	if s.getByIDFn != nil {
		return s.getByIDFn(ctx, id)
	}
	return nil, domain.ErrNotFound
}

func (s *stubAvatarService) ListByUserID(ctx context.Context, userID string) ([]*domain.Avatar, error) {
	if s.listFn != nil {
		return s.listFn(ctx, userID)
	}
	return nil, nil
}

func (s *stubAvatarService) GetContent(ctx context.Context, id uuid.UUID, size string) (io.ReadCloser, *domain.ObjectInfo, error) {
	if s.getContentFn != nil {
		return s.getContentFn(ctx, id, size)
	}
	return nil, nil, domain.ErrNotFound
}

func (s *stubAvatarService) Delete(ctx context.Context, id uuid.UUID, requesterID string) error {
	if s.deleteFn != nil {
		return s.deleteFn(ctx, id, requesterID)
	}
	return nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

// jpegData — минимальные данные с корректной сигнатурой JPEG.
var jpegData = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}

// newAvatarRouter собирает chi-роутер с теми же путями, что и реальный.
func newAvatarRouter(h *AvatarHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/avatars", h.Upload)
	r.Get("/avatars/{avatar_id}", h.GetByID)
	r.Get("/avatars/{avatar_id}/content", h.GetContent)
	r.Get("/avatars/{avatar_id}/metadata", h.Metadata)
	r.Delete("/avatars/{avatar_id}", h.Delete)
	r.Get("/users/{user_id}/avatar", h.GetUserAvatar)
	r.Delete("/users/{user_id}/avatar", h.DeleteUserAvatar)
	r.Get("/users/{user_id}/avatars", h.ListByUser)
	return r
}

// newTestHandler создаёт хендлер со стабом и тихим логгером.
func newTestHandler(svc AvatarService) *AvatarHandler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewAvatarHandler(svc, log)
}

// multipartRequest собирает POST-запрос с multipart-телом.
// fieldName == "" — тело без файла.
func multipartRequest(t *testing.T, target, fieldName, fileName string, data []byte) *http.Request {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if fieldName != "" {
		fw, err := mw.CreateFormFile(fieldName, fileName)
		require.NoError(t, err)
		_, err = fw.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, target, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// withUserID кладёт userID в контекст запроса (как это делает middleware UserID).
func withUserID(req *http.Request, userID string) *http.Request {
	return req.WithContext(httpx.WithUserID(req.Context(), userID))
}

// sampleAvatar возвращает заполненную доменную модель для ответов.
func sampleAvatar() *domain.Avatar {
	now := time.Now().UTC().Truncate(time.Second)
	return &domain.Avatar{
		ID:               uuid.New(),
		UserID:           "user-123",
		FileName:         "photo.jpg",
		MimeType:         "image/jpeg",
		SizeBytes:        int64(len(jpegData)),
		S3Key:            "originals/user-123/x/photo.jpg",
		ThumbnailS3Keys:  map[string]string{"300x300": "thumbnails/u/a/300x300.jpg", "100x100": "thumbnails/u/a/100x100.jpg"},
		UploadStatus:     domain.UploadStatusUploaded,
		ProcessingStatus: domain.ProcessingStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// ─── Upload ─────────────────────────────────────────────────────────────────

func TestAvatarHandler_Upload_Success(t *testing.T) {
	avatar := sampleAvatar()
	svc := &stubAvatarService{
		uploadFn: func(_ context.Context, userID, fileName, contentType string, data []byte) (*domain.Avatar, error) {
			assert.Equal(t, "user-123", userID)
			assert.Equal(t, "photo.jpg", fileName)
			assert.Equal(t, "image/jpeg", contentType)
			assert.Equal(t, jpegData, data)
			return avatar, nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(multipartRequest(t, "/avatars", "image", "photo.jpg", jpegData), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))

	var resp UploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, avatar.ID.String(), resp.ID)
	assert.Equal(t, "user-123", resp.UserID)
	assert.Equal(t, "/api/v1/avatars/"+avatar.ID.String(), resp.URL)
	assert.Equal(t, "processing", resp.Status)
	assert.False(t, resp.CreatedAt.IsZero())
}

func TestAvatarHandler_Upload_MissingFile(t *testing.T) {
	router := newAvatarRouter(newTestHandler(&stubAvatarService{}))

	// Multipart без поля "image".
	req := withUserID(multipartRequest(t, "/avatars", "", "", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"Missing file","details":"Field 'image' is required"}`, rec.Body.String())
}

func TestAvatarHandler_Upload_InvalidFormat(t *testing.T) {
	svc := &stubAvatarService{
		uploadFn: func(context.Context, string, string, string, []byte) (*domain.Avatar, error) {
			return nil, domain.ErrInvalidFormat
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(multipartRequest(t, "/avatars", "image", "photo.jpg", jpegData), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"Invalid file format","details":"Supported formats: jpeg, png, webp"}`, rec.Body.String())
}

func TestAvatarHandler_Upload_FileTooLarge(t *testing.T) {
	svc := &stubAvatarService{
		uploadFn: func(context.Context, string, string, string, []byte) (*domain.Avatar, error) {
			return nil, domain.ErrFileTooLarge
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(multipartRequest(t, "/avatars", "image", "photo.jpg", jpegData), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.JSONEq(t, `{"error":"File too large","max_size":10485760}`, rec.Body.String())
}

func TestAvatarHandler_Upload_ServiceError(t *testing.T) {
	svc := &stubAvatarService{
		uploadFn: func(context.Context, string, string, string, []byte) (*domain.Avatar, error) {
			return nil, assert.AnError
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(multipartRequest(t, "/avatars", "image", "photo.jpg", jpegData), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"Internal Server Error"}`, rec.Body.String())
}

// ─── GetByID ────────────────────────────────────────────────────────────────

func TestAvatarHandler_GetByID_Success(t *testing.T) {
	avatar := sampleAvatar()
	svc := &stubAvatarService{
		getByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Avatar, error) {
			assert.Equal(t, avatar.ID, id)
			return avatar, nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+avatar.ID.String(), nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp AvatarResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, avatar.ID.String(), resp.ID)
	assert.Equal(t, "user-123", resp.UserID)
	assert.Equal(t, "photo.jpg", resp.FileName)
	assert.Equal(t, "image/jpeg", resp.MimeType)
	assert.Equal(t, int64(len(jpegData)), resp.Size)
	assert.Equal(t, "/api/v1/avatars/"+avatar.ID.String(), resp.URL)
	assert.Equal(t, "uploaded", resp.Status)
	assert.Equal(t, "pending", resp.ProcessingStatus)

	// Миниатюры отсортированы по размеру.
	require.Len(t, resp.Thumbnails, 2)
	assert.Equal(t, "100x100", resp.Thumbnails[0].Size)
	assert.Equal(t, "300x300", resp.Thumbnails[1].Size)
}

func TestAvatarHandler_GetByID_InvalidUUID(t *testing.T) {
	router := newAvatarRouter(newTestHandler(&stubAvatarService{}))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/not-a-uuid", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"Invalid avatar ID"}`, rec.Body.String())
}

func TestAvatarHandler_GetByID_NotFound(t *testing.T) {
	svc := &stubAvatarService{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return nil, domain.ErrNotFound
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+uuid.NewString(), nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"Avatar not found"}`, rec.Body.String())
}

// ─── Metadata ───────────────────────────────────────────────────────────────

func TestAvatarHandler_Metadata_Success(t *testing.T) {
	avatar := sampleAvatar()
	svc := &stubAvatarService{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+avatar.ID.String()+"/metadata", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp MetadataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, avatar.ID.String(), resp.ID)
	assert.Equal(t, "photo.jpg", resp.FileName)
	assert.Equal(t, "image/jpeg", resp.MimeType)
	assert.Equal(t, int64(len(jpegData)), resp.Size)
	assert.Equal(t, 0, resp.Dimensions.Width)
	assert.Equal(t, 0, resp.Dimensions.Height)
	require.Len(t, resp.Thumbnails, 2)
	assert.Equal(t, "100x100", resp.Thumbnails[0].Size)
}

// ─── ListByUser ─────────────────────────────────────────────────────────────

func TestAvatarHandler_ListByUser_Success(t *testing.T) {
	svc := &stubAvatarService{
		listFn: func(_ context.Context, userID string) ([]*domain.Avatar, error) {
			assert.Equal(t, "user-123", userID)
			return []*domain.Avatar{sampleAvatar(), sampleAvatar()}, nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/users/user-123/avatars", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp []AvatarResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
}

// ─── GetContent ─────────────────────────────────────────────────────────────

// contentStub — стаб сервиса, отдающий заданные байты и метаданные.
func contentStub(t *testing.T, wantSize, contentType, etag string, body []byte) *stubAvatarService {
	t.Helper()
	return &stubAvatarService{
		getContentFn: func(_ context.Context, _ uuid.UUID, size string) (io.ReadCloser, *domain.ObjectInfo, error) {
			assert.Equal(t, wantSize, size)
			return io.NopCloser(bytes.NewReader(body)), &domain.ObjectInfo{
				Size:        int64(len(body)),
				ContentType: contentType,
				ETag:        etag,
			}, nil
		},
	}
}

func TestAvatarHandler_GetContent_Original(t *testing.T) {
	body := []byte("original-bytes")
	svc := contentStub(t, sizeOriginal, "image/jpeg", "abc123", body)
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+uuid.NewString()+"/content?size=original", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Equal(t, strconv.Itoa(len(body)), rec.Header().Get("Content-Length"))
	assert.Equal(t, contentCacheControl, rec.Header().Get("Cache-Control"))
	assert.Equal(t, `"abc123"`, rec.Header().Get("ETag"))
	assert.Equal(t, body, rec.Body.Bytes())
}

func TestAvatarHandler_GetContent_DefaultSizeIsOriginal(t *testing.T) {
	svc := contentStub(t, sizeOriginal, "image/jpeg", "", []byte("x"))
	router := newAvatarRouter(newTestHandler(svc))

	// Без параметра size сервис должен получить "original".
	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+uuid.NewString()+"/content", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// ETag пустой — заголовок не выставляем, чтобы не отдавать ETag: "".
	assert.Empty(t, rec.Header().Get("ETag"))
}

func TestAvatarHandler_GetContent_Thumbnail(t *testing.T) {
	body := []byte("thumb-bytes")
	svc := contentStub(t, "100x100", "image/jpeg", "thumb-etag", body)
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+uuid.NewString()+"/content?size=100x100", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Equal(t, body, rec.Body.Bytes())
}

func TestAvatarHandler_GetContent_NotImplemented(t *testing.T) {
	svc := &stubAvatarService{
		getContentFn: func(context.Context, uuid.UUID, string) (io.ReadCloser, *domain.ObjectInfo, error) {
			return nil, nil, domain.ErrNotImplemented
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+uuid.NewString()+"/content?size=500x500", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.JSONEq(t, `{"error":"Not Implemented","details":"available sizes: original, 100x100, 300x300"}`, rec.Body.String())
}

func TestAvatarHandler_GetContent_NotFound(t *testing.T) {
	svc := &stubAvatarService{
		getContentFn: func(context.Context, uuid.UUID, string) (io.ReadCloser, *domain.ObjectInfo, error) {
			return nil, nil, domain.ErrNotFound
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/"+uuid.NewString()+"/content", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"Avatar not found"}`, rec.Body.String())
}

func TestAvatarHandler_GetContent_InvalidUUID(t *testing.T) {
	router := newAvatarRouter(newTestHandler(&stubAvatarService{}))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/avatars/not-a-uuid/content", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"Invalid avatar ID"}`, rec.Body.String())
}

// ─── Delete ─────────────────────────────────────────────────────────────────

func TestAvatarHandler_Delete_Success(t *testing.T) {
	id := uuid.New()
	svc := &stubAvatarService{
		deleteFn: func(_ context.Context, gotID uuid.UUID, requesterID string) error {
			assert.Equal(t, id, gotID)
			assert.Equal(t, "user-123", requesterID)
			return nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodDelete, "/avatars/"+id.String(), nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestAvatarHandler_Delete_Forbidden(t *testing.T) {
	svc := &stubAvatarService{
		deleteFn: func(context.Context, uuid.UUID, string) error {
			return domain.ErrForbidden
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodDelete, "/avatars/"+uuid.NewString(), nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":"Forbidden","details":"You can only delete your own avatars"}`, rec.Body.String())
}

func TestAvatarHandler_Delete_NotFound(t *testing.T) {
	svc := &stubAvatarService{
		deleteFn: func(context.Context, uuid.UUID, string) error {
			return domain.ErrNotFound
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodDelete, "/avatars/"+uuid.NewString(), nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"Avatar not found"}`, rec.Body.String())
}

// ─── GetUserAvatar ──────────────────────────────────────────────────────────

func TestAvatarHandler_GetUserAvatar_Empty(t *testing.T) {
	svc := &stubAvatarService{
		listFn: func(context.Context, string) ([]*domain.Avatar, error) {
			return []*domain.Avatar{}, nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/users/user-123/avatar", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"Avatar not found"}`, rec.Body.String())
}

func TestAvatarHandler_GetUserAvatar_Success(t *testing.T) {
	avatar := sampleAvatar()
	svc := &stubAvatarService{
		listFn: func(context.Context, string) ([]*domain.Avatar, error) {
			return []*domain.Avatar{avatar}, nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodGet, "/users/user-123/avatar", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp AvatarResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, avatar.ID.String(), resp.ID)
}

// ─── DeleteUserAvatar ───────────────────────────────────────────────────────

func TestAvatarHandler_DeleteUserAvatar_Success(t *testing.T) {
	avatar := sampleAvatar()
	svc := &stubAvatarService{
		listFn: func(context.Context, string) ([]*domain.Avatar, error) {
			return []*domain.Avatar{avatar}, nil
		},
		deleteFn: func(_ context.Context, gotID uuid.UUID, requesterID string) error {
			assert.Equal(t, avatar.ID, gotID)
			assert.Equal(t, "user-123", requesterID)
			return nil
		},
	}
	router := newAvatarRouter(newTestHandler(svc))

	req := withUserID(httptest.NewRequest(http.MethodDelete, "/users/user-123/avatar", nil), "user-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}
