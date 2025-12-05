package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func processVideoForFastStart(filePath string) (string, error) {
	workFile := fmt.Sprintf("%s.processing", filePath)

	cmd := exec.Command(
		"ffmpeg",
		"-y",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		workFile,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		os.Remove(workFile)
		return "", fmt.Errorf("ffmpeg faststart failed: %w\nstderr: %s", err, stderr.String())
	}

	stat, err := os.Stat(workFile)
	if err != nil {
		return "", fmt.Errorf("ffmpeg produced no output file: %w", err)
	}
	if stat.Size() == 0 {
		os.Remove(workFile)
		return "", fmt.Errorf("ffmpeg output file is empty (input may be invalid)\nstderr: %s", stderr.String())
	}

	return workFile, nil
}

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

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	resp, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return resp.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return database.Video{}, fmt.Errorf("video URL is nil")
	}
	urlParts := strings.Split(*video.VideoURL, ",")
	if len(urlParts) != 2 {
		return database.Video{}, fmt.Errorf("invalid video URL format")
	}
	expireTime := 15 * time.Minute
	presignedURL, err := generatePresignedURL(cfg.s3Client, urlParts[0], urlParts[1], expireTime)
	if err != nil {
		return database.Video{}, err
	}
	video.VideoURL = &presignedURL
	return video, nil
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

		const epsilon = 0.02

		switch {
		case math.Abs(ratio-(16.0/9.0)) < epsilon:
			result = "16:9"
		case math.Abs(ratio-(9.0/16.0)) < epsilon:
			result = "9:16"
		default:
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

	io.Copy(tempFile, file)
	log.Println("finished copy", err)
	tempFile.Seek(0, io.SeekStart)
	randKey := make([]byte, 32)
	rand.Read(randKey)
	randFileName := base64.RawURLEncoding.EncodeToString(randKey)
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "aspectRatio error", err)
		return
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
	///preprocessing
	tempFile.Sync()

	tempFile.Close()
	fsVideo, err := processVideoForFastStart(tempFile.Name())
	log.Println("finished ffmpeg", err)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "", err)
		return
	}
	f, err := os.Open(fsVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "", err)
		return
	}
	defer f.Close()
	s3Params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        f,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), &s3Params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot put to s3", err)
		return
	}
	newURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, fileKey)
	video.UpdatedAt = time.Now()
	video.VideoURL = &newURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot load video to db", err)
		return
	}
	presignedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot presing the video", err)
	}

	respondWithJSON(w, http.StatusOK, presignedVideo)
}
