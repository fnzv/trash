package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// MediaHandler downloads Telegram media files and transcribes audio.
type MediaHandler struct {
	api        *tgbotapi.BotAPI
	workDir    string
	whisperCmd string
}

// DownloadFile downloads a Telegram file by fileID and saves it to workDir/media/.
// Returns the absolute path of the saved file.
func (m *MediaHandler) DownloadFile(fileID, ext string) (string, error) {
	file, err := m.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("get file metadata: %w", err)
	}

	url := file.Link(m.api.Token)
	log.Printf("[media] downloading %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download file: HTTP %d", resp.StatusCode)
	}

	mediaDir := filepath.Join(m.workDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}

	filename := fmt.Sprintf("%d_%d.%s", time.Now().UnixNano(), os.Getpid(), ext)
	path := filepath.Join(mediaDir, filename)

	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write file: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}

	log.Printf("[media] saved to %s", absPath)
	return absPath, nil
}

// TranscribeAudio runs the whisper CLI to transcribe an audio file.
// Returns the transcript text.
func (m *MediaHandler) TranscribeAudio(path string) (string, error) {
	dir := filepath.Dir(path)

	cmd := exec.Command(m.whisperCmd, path, "--model", "base", "--output_format", "txt", "--output_dir", dir)
	log.Printf("[media] running: %s", cmd.String())

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper failed: %w\noutput: %s", err, string(output))
	}

	// Whisper writes <basename>.txt in the output dir.
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	txtPath := filepath.Join(dir, base+".txt")

	transcript, err := os.ReadFile(txtPath)
	if err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
	}

	// Clean up the txt file.
	os.Remove(txtPath)

	text := strings.TrimSpace(string(transcript))
	log.Printf("[media] transcript (%d chars): %.200s", len(text), text)
	return text, nil
}

// Cleanup removes temporary media files.
func (m *MediaHandler) Cleanup(paths ...string) {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("[media] cleanup error: %v", err)
		} else {
			log.Printf("[media] cleaned up %s", p)
		}
	}
}
