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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type streams struct {
	Streams []stream `json:"streams"`
}

type stream struct {
	Index  int `json:"index"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	videoKey := make([]byte, 32)
	rand.Read(videoKey)
	encodedVideoKey := base64.RawURLEncoding.EncodeToString(videoKey)

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

	fmt.Println("uploading video", videoID, "by user", userID)

	const maxMemory = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusRequestEntityTooLarge, "The video file was too large", err)
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	fileExtension, mediaType, err := getVideoMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to parse MIME type.", err)
	}

	videoMetaData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video metadata not found", err)
		return
	}
	if videoMetaData.UserID != userID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	fmt.Println("Saving video with file extension: ", fileExtension)
	fullFilePath := path.Join(fmt.Sprintf("tubely-upload.%v", fileExtension))
	out, err := os.CreateTemp("", fullFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temporary file", err)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video to temporary file", err)
		return
	}
	defer os.Remove(fullFilePath)

	file.Seek(0, io.SeekStart)
	aspectRatio, err := getVideoAspectRatio(out.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get the video's aspect ratio", err)
		return
	}
	processedVideoPath, err := processVideoForFastStart(out.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to preprocess the video for fast start", err)
		return
	}
	processedFile, err := os.Open(processedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open the processed video file", err)
		return
	}
	defer processedFile.Close()

	fileKey := fmt.Sprintf("%v/%v.%v", aspectRatio, encodedVideoKey, fileExtension)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to send the file to S3", err)
		return
	}

	URL := fmt.Sprintf("%s,%s/%s.%s", cfg.s3Bucket, aspectRatio, encodedVideoKey, fileExtension)
	videoMetaData.VideoURL = &URL

	err = cfg.db.UpdateVideo(videoMetaData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetaData)
}

func getVideoMediaType(mediaType string) (string, string, error) {
	parsedType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return "", "", err
	}
	if parsedType == "video/mp4" {
		return "mp4", parsedType, nil
	}
	return "", "", fmt.Errorf("got an invalid video file type")
}

func getVideoAspectRatio(filePath string) (string, error) {
	fmt.Println("Running ffprobe with filepath: ", filePath)

	command := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out bytes.Buffer
	command.Stdout = &out
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("FFProbe command failed: %v, %v", err, out.String())
	}

	output := out.Bytes()

	var ffProbeOut streams
	if err := json.Unmarshal(output, &ffProbeOut); err != nil {
		return "", fmt.Errorf("failed to get the video dimensions from ffprobe reponse: %v", err)
	}

	aspectRatio := calculateAspectRatio(ffProbeOut.Streams[0].Width, ffProbeOut.Streams[0].Height)
	return aspectRatio, nil
}

func calculateAspectRatio(width, height int) string {
	ratio := float64(width) / float64(height)

	ratio16to9 := 16.0 / 9.0
	ratio9to16 := 9.0 / 16.0

	if almostEqual(ratio, ratio16to9, 0.2) {
		return "landscape"
	}
	if almostEqual(ratio, ratio9to16, 0.2) {
		return "portrait"
	}
	return "other"
}

func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := fmt.Sprintf("%s.processing", filePath)
	command := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("failed to pre-process the video for fast start: %v", err)
	}
	return outputFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s3Client)

	presignedURL, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL for the object: %v", err)
	}
	return presignedURL.URL, nil
}
