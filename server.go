package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YanxinTang/clipboard-online/utils"
	"github.com/gin-gonic/gin"
	"github.com/lxn/walk"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

const (
	apiVersion = "1"
	typeText   = "text"
	typeFile   = "file"
	typeMedia  = "media"
)

func setupRoute(engin *gin.Engine) {
	engin.Use(clientName(), requestID(), logger(), gin.Recovery(), apiVersionChecker())
	engin.GET("/", getHandler)
	engin.POST("/", setHandler)
	engin.NoRoute(notFoundHandler)
}

func clientName() gin.HandlerFunc {
	return func(c *gin.Context) {
		urlEncodedClientName := c.GetHeader("X-Client-Name")
		clientName, err := url.PathUnescape(urlEncodedClientName)
		if err != nil {
			clientName = "匿名设备"
		}
		if clientName == "" {
			clientName = "匿名设备"
		}
		c.Set("clientName", clientName)
		c.Next()
	}
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		randID := rand.Int()
		c.Set("requestID", strconv.Itoa(randID))
		c.Next()
	}
}

func apiVersionChecker() gin.HandlerFunc {
	return func(c *gin.Context) {
		version := c.GetHeader("X-API-Version")
		if version == apiVersion {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "接口版本不匹配，请升级您的捷径",
		})
	}
}

func logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		start := time.Now()
		c.Next()
		duration := time.Since(start)
		clientIP := c.ClientIP()
		statusCode := c.Writer.Status()
		requestID := c.GetString("requestID")
		clientName := c.GetString("clientName")
		requestLogger := log.WithFields(logrus.Fields{
			"requestID":  requestID,
			"method":     c.Request.Method,
			"statusCode": statusCode,
			"clientIP":   clientIP,
			"path":       path,
			"duration":   duration,
			"clientName": clientName,
		})

		if statusCode >= http.StatusInternalServerError {
			requestLogger.Error()
		} else if statusCode >= http.StatusBadRequest {
			requestLogger.Warn()
		} else {
			requestLogger.Info()
		}
	}
}

type ResponseFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ResponseFiles []ResponseFile

func getHandler(c *gin.Context) {
	logger := log.WithField("requestID", c.GetString("requestID"))
	contentType, err := utils.Clipboard().ContentType()
	if err != nil {
		logger.WithError(err).Info("failed to get content type of clipboard")
		c.Status(http.StatusBadRequest)
		return
	}

	if contentType == typeText {
		str, err := walk.Clipboard().Text()
		if err != nil {
			c.Status(http.StatusBadRequest)
			logger.WithError(err).Warn("failed to get clipboard")
			return
		}
		logger.Info("get clipboard text")
		c.JSON(http.StatusOK, gin.H{
			"type": "text",
			"data": str,
		})
		defer sendCopyNotification(logger, c.GetString("clientName"), str)
		return
	}

	if contentType == typeFile {
		// get path of files from clipboard
		filenames, err := utils.Clipboard().Files()
		if err != nil {
			logger.WithError(err).Warn("failed to get path of files from clipboard")
			c.Status(http.StatusBadRequest)
			return
		}

		responseFiles := make([]ResponseFile, 0, len(filenames))
		for _, path := range filenames {
			base64, err := readBase64FromFile(path)
			if err != nil {
				log.WithError(err).WithField("filepath", path).Warning("read base64 from file failed")
				continue
			}
			responseFiles = append(responseFiles, ResponseFile{filepath.Base(path), base64})
		}
		logger.Info("get clipboard files")

		c.JSON(http.StatusOK, gin.H{
			"type": "file",
			"data": responseFiles,
		})
		defer sendCopyNotification(logger, c.GetString("clientName"), "[文件] 被复制")
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "无法识别剪切板内容"})
}

func readBase64FromFile(path string) (string, error) {
	fileBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(fileBytes), nil
}

// Set clipboard handler

// TextBody is a struct of request body when iOS send files to windows
type TextBody struct {
	Text string `json:"text"`
}

func setHandler(c *gin.Context) {
	requestLogger := log.WithField("requestID", c.GetString("requestID"))
	contentType := c.GetHeader("X-Content-Type")
	if contentType == typeText {
		setTextHandler(c, requestLogger)
		return
	}

	setFileHandler(c, requestLogger)
}

func setTextHandler(c *gin.Context, logger *logrus.Entry) {
	var body TextBody
	if err := c.ShouldBindJSON(&body); err != nil {
		logger.WithError(err).Warn("failed to bind file body")
		c.Status(http.StatusBadRequest)
		return
	}

	if err := utils.Clipboard().SetText(body.Text); err != nil {
		logger.WithError(err).Warn("failed to set clipboard")
		c.Status(http.StatusBadRequest)
		return
	}

	var notify string = "粘贴内容为空"
	if body.Text != "" {
		notify = body.Text
	}
	defer sendPasteNotification(logger, c.GetString("clientName"), notify)
	logger.WithField("text", body.Text).Info("set clipboard text")
	c.Status(http.StatusOK)
}

// FileBody is a struct of request body when iOS send files to windows
type FileBody struct {
	NamesString        string `json:"names"` // Name1\nName2...
	EncodedFilesString string `json:"files"` // File1Base64\nFile2Base64...
}

// ByteFile is a struct to save uploaded file in memory
type ByteFile struct {
	Name  string // filename
	Bytes []byte // bytes of file content
}

// ByteFiles returns ByteFile list from request body
func (fb *FileBody) ByteFiles() ([]ByteFile, error) {
	names := strings.Split(fb.NamesString, "\n")
	encodedFiles := strings.Split(fb.EncodedFilesString, "\n")

	byteFiles := make([]ByteFile, 0, len(encodedFiles))
	for i, encodedFile := range encodedFiles {
		fileBytes, err := base64.StdEncoding.DecodeString(encodedFile)
		if err != nil {
			// todo: warning log to file
			continue
		}
		byteFiles = append(byteFiles, ByteFile{names[i], fileBytes})
	}
	return byteFiles, nil
}

func setFileHandler(c *gin.Context, logger *logrus.Entry) {
	contentType := c.GetHeader("X-Content-Type")
	cleanTempFiles(logger)

	var body FileBody
	if err := c.ShouldBindJSON(&body); err != nil {
		logger.WithError(err).Warn("failed to bind file body")
		c.Status(http.StatusBadRequest)
		return
	}

	byteFiles, err := body.ByteFiles()
	if err != nil {
		logger.WithError(err).Warn("failed to get files from request")
		c.Status(http.StatusBadRequest)
		return
	}
	paths := make([]string, 0, len(byteFiles))
	for _, byteFile := range byteFiles {
		path := getTempFilePath(string(byteFile.Name))
		if err := ioutil.WriteFile(path, byteFile.Bytes, 0644); err != nil {
			logger.WithError(err).WithField("path", path).Warn("failed to create file")
			continue
		}
		logger.WithField("path", path).Debug()
		paths = append(paths, path)
	}
	// write paths to file
	setLastFilenames(paths)

	if err := utils.Clipboard().SetFiles(paths); err != nil {
		logger.WithError(err).Warn("failed to set clipboard")
		c.Status(http.StatusBadRequest)
		return
	}

	var notify string
	if contentType == typeMedia {
		notify = "[图片媒体] 已复制到剪贴板"
	} else {
		notify = "[文件] 已复制到剪贴板"
	}

	defer sendPasteNotification(logger, c.GetString("clientName"), notify)
	logger.WithField("paths", paths).Info("set clipboard file")
	c.Status(http.StatusOK)
}

func notFoundHandler(c *gin.Context) {
	requestLogger := log.WithFields(log.Fields{"request_id": rand.Int(), "user_ip": c.Request.RemoteAddr})
	requestLogger.Info("404 not found")
	c.Status(http.StatusNotFound)
}

func sendCopyNotification(logger *log.Entry, client, notify string) {
	sendNotification(logger, "复制", client, notify)
}

func sendPasteNotification(logger *log.Entry, client, notify string) {
	sendNotification(logger, "粘贴", client, notify)
}

func sendNotification(logger *log.Entry, action, client, notify string) {
	if notify == "" {
		notify = action + "内容为空"
	}
	title := fmt.Sprintf("%s自 %s", action, client)
	if err := app.ni.ShowInfo(title, notify); err != nil {
		logger.WithError(err).WithField("notify", notify).Warn("failed to send notification")
	}
}

func getTempFilePath(filename string) string {
	if !filepath.IsAbs(app.config.TempDir) {
		// temp files path in exec path but not pwd
		tempAbsPath := path.Join(execPath, app.config.TempDir)
		return filepath.Join(tempAbsPath, filename)
	}
	return filepath.Join(app.config.TempDir, filename)
}

func setLastFilenames(filenames []string) {
	path := getTempFilePath("_filename.txt")
	allFilenames := strings.Join(filenames, "\n")
	_ = ioutil.WriteFile(path, []byte(allFilenames), os.ModePerm)
}

func cleanTempFiles(logger *logrus.Entry) {
	tempDir := getTempFilePath("")
	if a, err := os.Stat(tempDir); err != nil || !a.IsDir() {
		_ = os.Mkdir(tempDir, os.ModePerm)
	}

	path := getTempFilePath("_filename.txt")
	if isExistFile(path) {
		file, err := os.Open(path)
		if err != nil {
			logger.WithError(err).WithField("path", path).Warn("failed to open temp file")
			return
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			delPath := scanner.Text()
			if err = os.Remove(delPath); err != nil {
				logger.WithError(err).WithField("delPath", delPath).Warn("failed to delete specify path")
			}
		}
	}
}
