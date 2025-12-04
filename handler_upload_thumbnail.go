package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func mimeCheckImage(mimeType string) error {
	m, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return err
	}
	switch m {
	case "image/jpeg":
	case "image/png":
	default:
		return fmt.Errorf("not supported mimetype")
	}
	return nil
}

func mimeToExt(mimeType string) string {
	parts := strings.Split(mimeType, "/")
	return parts[len(parts)-1]
}

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// TODO: implement the upload here
	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse from file", err)
		return
	}
	defer file.Close()
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "not authorized", nil)
		return
	}
	mediaType := header.Header.Get("Content-Type")
	if err = mimeCheckImage(mediaType); err != nil {
		respondWithError(w, http.StatusBadRequest, "", err)
	}
	ext := mimeToExt(mediaType)

	randKey := make([]byte, 32)
	rand.Read(randKey)
	randFileName := base64.RawURLEncoding.EncodeToString(randKey)

	assetPath := fmt.Sprintf("%s.%s", randFileName, ext)
	assetDiskPath := filepath.Join(cfg.assetsRoot, assetPath)
	osFile, err := os.Create(assetDiskPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot create file", err)
		return
	}
	_, err = io.Copy(osFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot write to file", err)
	}
	url := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, assetPath)
	video.ThumbnailURL = &url
	video.UpdatedAt = time.Now()

	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "cant update video thumbnail", err)
		return
	}
	video, err = cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
