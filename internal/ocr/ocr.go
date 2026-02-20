package ocr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ExtractText runs OCR on the given file and returns extracted text.
// For PDFs, converts the first page to PNG via pdftoppm first.
// For images, runs tesseract directly.
func ExtractText(filePath, docType string) (string, error) {
	docType = strings.ToLower(docType)

	switch docType {
	case ".pdf":
		return extractFromPDF(filePath)
	case ".png", ".jpg", ".jpeg", ".tiff", ".bmp":
		return extractFromImage(filePath)
	default:
		return "", fmt.Errorf("unsupported document type for OCR: %s", docType)
	}
}

func extractFromPDF(pdfPath string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "godocs-ocr-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Convert first page to PNG
	outPrefix := filepath.Join(tmpDir, "page")
	cmd := exec.Command("pdftoppm", "-png", "-f", "1", "-l", "1", "-singlefile", pdfPath, outPrefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm failed: %w: %s", err, string(out))
	}

	pngPath := outPrefix + ".png"
	return extractFromImage(pngPath)
}

func extractFromImage(imagePath string) (string, error) {
	cmd := exec.Command("tesseract", imagePath, "stdout")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tesseract failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
