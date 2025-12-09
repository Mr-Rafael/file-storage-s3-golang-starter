package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	fileExtension, err := getImageFileExtension(header.Header.Get("Content-Type"))
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

	fullFilePath := path.Join(cfg.assetsRoot, fmt.Sprintf("%v.%v", encodedVideoKey, fileExtension))
	out, err := os.Create(fullFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create file", err)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save thumbnail to file", err)
		return
	}

	URL := fmt.Sprintf("http://localhost:8091/assets/%v.%v", encodedVideoKey, fileExtension)
	videoMetaData.ThumbnailURL = &URL

	err = cfg.db.UpdateVideo(videoMetaData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetaData)
}

func getImageFileExtension(mediaType string) (string, error) {
	parsedType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return "", err
	}
	if parsedType == "image/jpeg" {
		return "jpg", nil
	}
	if parsedType == "image/png" {
		return "png", nil
	}
	return "", fmt.Errorf("got an invalid image file type")
}
