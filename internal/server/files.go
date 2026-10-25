package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/store"
	"github.com/ahyield/eze/internal/uploads"
)

// multipartOverhead allows for boundaries and part headers on top of the file itself.
const multipartOverhead = 64 << 10

// Uploaded files run no code: they are sandboxed even when opened directly, so an uploaded
// HTML or SVG file can't act as the site, and anything not known to be safe downloads.
const fileCSP = "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'"

type fileResponse struct {
	store.File
	URL string `json:"url"`
}

func fileURL(f *store.File) string {
	return "/_eze/files/" + f.ID + "/" + url.PathEscape(f.Name)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	site := siteFrom(r.Context())
	used, err := s.store.UploadedBytes(r.Context(), site)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	room := s.cfg.UploadQuota - used
	if room <= 0 {
		writeErr(w, http.StatusRequestEntityTooLarge, "this site has used its upload quota; delete some files first")
		return
	}
	limit := min(s.cfg.MaxUploadBytes, room)

	r.Body = http.MaxBytesReader(w, r.Body, limit+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "upload must be multipart/form-data with a \"file\" field")
		return
	}
	var part io.ReadCloser
	var filename, declared string
	for {
		p, err := mr.NextPart()
		if err != nil {
			s.uploadReadErr(w, r, err, "no \"file\" field in upload")
			return
		}
		if p.FormName() == "file" {
			part, filename, declared = p, p.FileName(), p.Header.Get("Content-Type")
			break
		}
		p.Close()
	}
	defer part.Close()

	id := store.NewID()
	size, head, err := s.uploads.Save(r.Context(), site, id, part, limit)
	if errors.Is(err, uploads.ErrTooLarge) {
		if limit < s.cfg.MaxUploadBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "file is larger than this site's remaining upload quota")
		} else {
			writeErr(w, http.StatusRequestEntityTooLarge, "file is larger than "+strconv.FormatInt(s.cfg.MaxUploadBytes, 10)+" bytes")
		}
		return
	}
	if err != nil {
		s.uploadReadErr(w, r, err, "")
		return
	}

	name := uploads.CleanName(filename)
	f := &store.File{
		ID:          id,
		Name:        name,
		ContentType: uploads.DetectType(name, declared, head),
		Size:        size,
		CreatedBy:   auth.UserFrom(r.Context()).Email,
		CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.store.CreateFile(r.Context(), site, f); err != nil {
		_ = s.uploads.Delete(context.WithoutCancel(r.Context()), site, id) // best effort; report the store error
		s.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, fileResponse{*f, fileURL(f)})
}

// uploadReadErr maps errors from reading the request body. Errors that aren't about the
// request (disk full, permissions) are internal.
func (s *Server) uploadReadErr(w http.ResponseWriter, r *http.Request, err error, badRequest string) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeErr(w, http.StatusRequestEntityTooLarge, "upload too large")
	case errors.Is(err, io.EOF) && badRequest != "":
		writeErr(w, http.StatusBadRequest, badRequest)
	case errors.Is(err, io.ErrUnexpectedEOF), badRequest != "":
		writeErr(w, http.StatusBadRequest, "malformed upload")
	default:
		s.internalErr(w, r, err)
	}
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	limit, offset := defaultListLimit, 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxListOffset {
			writeErr(w, http.StatusBadRequest, "offset must be between 0 and 100000")
			return
		}
		offset = n
	}
	files, err := s.store.ListFiles(r.Context(), siteFrom(r.Context()), limit, offset)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	out := make([]fileResponse, len(files))
	for i := range files {
		out[i] = fileResponse{files[i], fileURL(&files[i])}
	}
	writeJSON(w, http.StatusOK, out)
}

// fileFromPath loads the metadata for the {id} path value, answering 400/404 itself.
func (s *Server) fileFromPath(w http.ResponseWriter, r *http.Request) (*store.File, bool) {
	id := r.PathValue("id")
	if !store.ValidDocID(id) {
		writeErr(w, http.StatusBadRequest, "invalid file id")
		return nil, false
	}
	f, err := s.store.GetFile(r.Context(), siteFrom(r.Context()), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "file not found")
		return nil, false
	}
	if err != nil {
		s.internalErr(w, r, err)
		return nil, false
	}
	return f, true
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.fileFromPath(w, r); ok {
		writeJSON(w, http.StatusOK, fileResponse{*f, fileURL(f)})
	}
}

// handleFileContent serves the bytes. The {name} segment only makes URLs readable and
// downloads well-named; the id alone identifies the file.
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	f, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	file := s.uploads.Open(r.Context(), siteFrom(r.Context()), f.ID, f.Size)
	defer file.Close()

	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Security-Policy", fileCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	// Files never change once uploaded, so browsers can keep them; private because they're behind sign-in.
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	disposition := "attachment"
	if uploads.Inline(f.ContentType) {
		disposition = "inline"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}))
	http.ServeContent(w, r, "", f.CreatedAt, file)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !store.ValidDocID(id) {
		writeErr(w, http.StatusBadRequest, "invalid file id")
		return
	}
	site := siteFrom(r.Context())
	err := s.store.DeleteFile(r.Context(), site, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	if err := s.uploads.Delete(r.Context(), site, id); err != nil {
		s.internalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
