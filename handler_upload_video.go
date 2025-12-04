package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func mimeCheckVideo(mimeType string) error {
	m, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return err
	}
	switch m {
	case "video/mp4":
	default:
		return fmt.Errorf("not supported mimetype")
	}
	return nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	type FFProbeOutput struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("unable to run ffprobe %w %s", err, stderr.String())
	}
	var jsonFFP FFProbeOutput
	if err := json.Unmarshal(out.Bytes(), &jsonFFP); err != nil {
		return "", fmt.Errorf("unmarshal error %w", err)
	}
	var result string
	if len(jsonFFP.Streams) > 0 {
		w := float64(jsonFFP.Streams[0].Width)
		h := float64(jsonFFP.Streams[0].Height)

		ratio := w / h

		const epsilon = 0.02 // adjust as needed; 2% tolerance

		switch {
		case math.Abs(ratio-(16.0/9.0)) < epsilon:
			result = "16:9"
		case math.Abs(ratio-(9.0/16.0)) < epsilon:
			result = "9:16"
		default:
			// fallback to exact reduced ratio
			// (your GCD code)
			a, b := int(w), int(h)
			for b != 0 {
				a, b = b, a%b
			}
			g := a
			result = fmt.Sprintf("%d:%d", int(w)/g, int(h)/g)
		}
	}

	return result, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	videoID := path.Base(r.URL.String())
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
	video, err := cfg.db.GetVideo(uuid.MustParse(videoID))
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "not video owner", err)
		return
	}
	if err != nil {
		respondWithError(w, http.StatusNotFound, "cant find video", err)
		return
	}
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error loading file", err)
		return
	}
	defer file.Close()
	mediaType := header.Header.Get("Content-Type")
	if err := mimeCheckVideo(mediaType); err != nil {
		respondWithError(w, http.StatusBadRequest, "not supported mimetype", err)
		return
	}
	tempFile, _ := os.CreateTemp("", "tubely-temp-upload.mp4")
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	io.Copy(tempFile, file)
	tempFile.Seek(0, io.SeekStart)
	randKey := make([]byte, 32)
	rand.Read(randKey)
	randFileName := base64.RawURLEncoding.EncodeToString(randKey)
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "aspectRatio error", err)
	}

	var fileKey string
	switch aspectRatio {
	case "16:9":
		fileKey = fmt.Sprintf("landscape/%s.%s", randFileName, mimeToExt(mediaType))
	case "9:16":
		fileKey = fmt.Sprintf("portrait/%s.%s", randFileName, mimeToExt(mediaType))
	default:
		fileKey = fmt.Sprintf("other/%s.%s", randFileName, mimeToExt(mediaType))
	}
	s3Params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        tempFile,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), &s3Params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot put to s3", err)
		return
	}
	newURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileKey)
	video.UpdatedAt = time.Now()
	video.VideoURL = &newURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot load video to db", err)
	}
	respondWithJSON(w, http.StatusOK, "")
}
