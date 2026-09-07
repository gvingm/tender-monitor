package main

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode"
)

const (
	maxExtractTotalSize = 500 * 1024 * 1024 // 500 МБ суммарно распакованных файлов
	maxExtractFiles     = 50                // максимум файлов из одного архива
)

// extractedFile — файл, извлечённый из архива (временный; вызывающий обязан удалить).
type extractedFile struct {
	path string
	name string
	size int64
}

// isArchive определяет по расширению, является ли файл архивом.
func isArchive(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".zip", ".rar", ".7z", ".tar", ".gz", ".tgz", ".bz2", ".tar.gz"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// extractArchive распаковывает архив во временную директорию и возвращает список извлечённых файлов
// и путь к временной директории (вызывающий обязан удалить её через os.RemoveAll).
// Сам архив НЕ входит в результат. При ошибке все временные файлы удаляются.
func extractArchive(ctx context.Context, archivePath, archiveName string) ([]extractedFile, string, error) {
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}

	var files []extractedFile
	var totalSize int64
	var tmpDir string

	log.Printf("[archive] распаковка %s (%s)", archiveName, archivePath)

	// очистка при ошибке
	cleanup := func() {
		for _, f := range files {
			os.Remove(f.path)
		}
		if tmpDir != "" {
			os.RemoveAll(tmpDir)
		}
	}

	// создаём временную директорию
	tmpDir, err := os.MkdirTemp(os.TempDir(), "tender-archive-*")
	if err != nil {
		return nil, "", fmt.Errorf("mkdirtemp: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(archiveName))
	var extractErr error
	switch ext {
	case ".zip":
		files, totalSize, extractErr = extractZip(archivePath, tmpDir)
	case ".rar":
		files, totalSize, extractErr = extractRar(archivePath, tmpDir)
	case ".7z":
		files, totalSize, extractErr = extract7z(archivePath, tmpDir)
	default:
		if strings.HasSuffix(strings.ToLower(archiveName), ".tar.gz") || strings.HasSuffix(strings.ToLower(archiveName), ".tgz") {
			files, totalSize, extractErr = extractTarGz(archivePath, tmpDir)
		} else if ext == ".tar" {
			files, totalSize, extractErr = extractTar(archivePath, tmpDir)
		} else if ext == ".gz" || ext == ".tgz" {
			files, totalSize, extractErr = extractTarGz(archivePath, tmpDir)
		} else if ext == ".bz2" {
			files, totalSize, extractErr = extractTarBz2(archivePath, tmpDir)
		} else {
			cleanup()
			return nil, "", fmt.Errorf("неподдерживаемый формат архива: %s", ext)
		}
	}

	if extractErr != nil {
		cleanup()
		return nil, "", extractErr
	}

	if totalSize > maxExtractTotalSize {
		cleanup()
		return nil, "", fmt.Errorf("превышен лимит распаковки: %d > %d байт", totalSize, maxExtractTotalSize)
	}

	log.Printf("[archive] %s — распаковано %d файлов, суммарный размер %d", archiveName, len(files), totalSize)
	return files, tmpDir, nil
}

// extractZip распаковывает ZIP.
func extractZip(archivePath, destDir string) ([]extractedFile, int64, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, 0, fmt.Errorf("zip open: %w", err)
	}
	defer r.Close()

	var files []extractedFile
	var totalSize int64

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if len(files) >= maxExtractFiles {
			return nil, 0, fmt.Errorf("превышен лимит файлов в архиве (%d)", maxExtractFiles)
		}
		if f.UncompressedSize64 > uint64(maxExtractTotalSize) {
			return nil, 0, fmt.Errorf("файл в архиве слишком большой: %s", f.Name)
		}

		name := sanitizeFileName(filepath.Base(f.Name))
		if name == "" || name == "." {
			name = "file"
		}
		tmpPath := filepath.Join(destDir, fmt.Sprintf("%d_%s", len(files), name))

		rc, err := f.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("zip open %s: %w", f.Name, err)
		}
		size, err := writeToFile(rc, tmpPath)
		rc.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("zip extract %s: %w", f.Name, err)
		}

		files = append(files, extractedFile{path: tmpPath, name: name, size: size})
		totalSize += size
	}

	return files, totalSize, nil
}

// extractRar распаковывает RAR.
func extractRar(archivePath, destDir string) ([]extractedFile, int64, error) {
	r, err := rardecode.OpenReader(archivePath, "")
	if err != nil {
		return nil, 0, fmt.Errorf("rar open: %w", err)
	}
	defer r.Close()

	var files []extractedFile
	var totalSize int64

	for {
		header, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("rar read: %w", err)
		}
		if header.IsDir {
			continue
		}
		if len(files) >= maxExtractFiles {
			return nil, 0, fmt.Errorf("превышен лимит файлов в архиве (%d)", maxExtractFiles)
		}
		if header.UnPackedSize > int64(maxExtractTotalSize) {
			return nil, 0, fmt.Errorf("файл в архиве слишком большой: %s", header.Name)
		}

		name := sanitizeFileName(filepath.Base(header.Name))
		if name == "" || name == "." {
			name = "file"
		}
		tmpPath := filepath.Join(destDir, fmt.Sprintf("%d_%s", len(files), name))

		size, err := writeToFile(r, tmpPath)
		if err != nil {
			return nil, 0, fmt.Errorf("rar extract %s: %w", header.Name, err)
		}

		files = append(files, extractedFile{path: tmpPath, name: name, size: size})
		totalSize += size
	}

	return files, totalSize, nil
}

// extract7z распаковывает 7z.
func extract7z(archivePath, destDir string) ([]extractedFile, int64, error) {
	r, err := sevenzip.OpenReader(archivePath)
	if err != nil {
		return nil, 0, fmt.Errorf("7z open: %w", err)
	}
	defer r.Close()

	var files []extractedFile
	var totalSize int64

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if len(files) >= maxExtractFiles {
			return nil, 0, fmt.Errorf("превышен лимит файлов в архиве (%d)", maxExtractFiles)
		}
		if f.UncompressedSize > uint64(maxExtractTotalSize) {
			return nil, 0, fmt.Errorf("файл в архиве слишком большой: %s", f.Name)
		}

		name := sanitizeFileName(filepath.Base(f.Name))
		if name == "" || name == "." {
			name = "file"
		}
		tmpPath := filepath.Join(destDir, fmt.Sprintf("%d_%s", len(files), name))

		rc, err := f.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("7z open %s: %w", f.Name, err)
		}
		size, err := writeToFile(rc, tmpPath)
		rc.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("7z extract %s: %w", f.Name, err)
		}

		files = append(files, extractedFile{path: tmpPath, name: name, size: size})
		totalSize += size
	}

	return files, totalSize, nil
}

// writeToFile записывает данные из reader во временный файл и возвращает размер.
func writeToFile(r io.Reader, path string) (int64, error) {
	out, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, r)
}

// extractTar распаковывает TAR.
func extractTar(archivePath, destDir string) ([]extractedFile, int64, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, 0, fmt.Errorf("tar open: %w", err)
	}
	defer f.Close()
	return extractTarReader(f, destDir)
}

// extractTarGz распаковывает TAR.GZ / TGZ.
func extractTarGz(archivePath, destDir string) ([]extractedFile, int64, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, 0, fmt.Errorf("targz open: %w", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, 0, fmt.Errorf("targz gzip: %w", err)
	}
	defer gr.Close()
	return extractTarReader(gr, destDir)
}

// extractTarBz2 распаковывает TAR.BZ2.
func extractTarBz2(archivePath, destDir string) ([]extractedFile, int64, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, 0, fmt.Errorf("tarbz2 open: %w", err)
	}
	defer f.Close()
	return extractTarReader(bzip2.NewReader(f), destDir)
}

// extractTarReader — общая логика для tar, tar.gz, tar.bz2.
func extractTarReader(r io.Reader, destDir string) ([]extractedFile, int64, error) {
	tr := tar.NewReader(r)
	var files []extractedFile
	var totalSize int64

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("tar read: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if len(files) >= maxExtractFiles {
			return nil, 0, fmt.Errorf("превышен лимит файлов в архиве (%d)", maxExtractFiles)
		}
		if header.Size > maxExtractTotalSize {
			return nil, 0, fmt.Errorf("файл в архиве слишком большой: %s", header.Name)
		}

		name := sanitizeFileName(filepath.Base(header.Name))
		if name == "" || name == "." {
			name = "file"
		}
		tmpPath := filepath.Join(destDir, fmt.Sprintf("%d_%s", len(files), name))

		size, err := writeToFile(tr, tmpPath)
		if err != nil {
			return nil, 0, fmt.Errorf("tar extract %s: %w", header.Name, err)
		}

		files = append(files, extractedFile{path: tmpPath, name: name, size: size})
		totalSize += size
	}

	return files, totalSize, nil
}
