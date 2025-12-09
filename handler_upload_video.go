package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

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
	fileKey := fmt.Sprintf("%v.%v", encodedVideoKey, fileExtension)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        file,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to send the file to S3", err)
		return
	}

	URL := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, fileKey)
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
