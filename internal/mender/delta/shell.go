package delta

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ShellGzip compresses a file using system gzip command
// This is significantly faster than Go's compress/gzip on ARM hardware
func ShellGzip(inputFile, outputFile string, level int) error {
	outFile, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	cmd := exec.Command("gzip", fmt.Sprintf("-%d", level), "-c", inputFile)
	cmd.Stdout = outFile
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gzip failed: %w", err)
	}

	return nil
}

// ShellGunzip decompresses a file using system gunzip command
func ShellGunzip(inputFile, outputFile string) error {
	return ShellGunzipWithProgress(inputFile, outputFile, 0, 0, 0, nil)
}

// ShellGunzipWithProgress decompresses with progress reporting based on output bytes.
// expectedOutput is the approximate decompressed size (0 to skip progress).
func ShellGunzipWithProgress(inputFile, outputFile string, expectedOutput int64, pctStart, pctEnd int, progress ProgressCallback) error {
	outFile, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	cmd := exec.Command("gunzip", "-c", inputFile)
	cmd.Stderr = os.Stderr

	if expectedOutput > 0 && progress != nil {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("gunzip stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("gunzip start: %w", err)
		}
		pr := newProgressReader(stdout, expectedOutput, pctStart, pctEnd, "decompressing", progress)
		if _, err := copyFromReader(outFile, pr); err != nil {
			cmd.Wait()
			return fmt.Errorf("gunzip copy: %w", err)
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("gunzip failed: %w", err)
		}
	} else {
		cmd.Stdout = outFile
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gunzip failed: %w", err)
		}
	}

	return nil
}

func copyFromReader(dst *os.File, src *progressReader) (int64, error) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			nw, werr := dst.Write(buf[:n])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

// ShellTarExtract extracts a tar archive using system tar command
func ShellTarExtract(tarFile, extractDir string) error {
	cmd := exec.Command("tar", "-xf", tarFile, "-C", extractDir)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extraction failed: %w, output: %s", err, output)
	}

	return nil
}

// ShellTarCreate creates a tar archive using system tar command
func ShellTarCreate(tarFile, sourceDir string) error {
	cmd := exec.Command("tar", "-cf", tarFile, "-C", sourceDir, ".")

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar creation failed: %w, output: %s", err, output)
	}

	return nil
}

// ShellSHA256 calculates SHA256 using system sha256sum command
func ShellSHA256(file string) (string, error) {
	cmd := exec.Command("sha256sum", file)

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("sha256sum failed: %w", err)
	}

	// sha256sum output format: "checksum  filename"
	// We just want the checksum part
	checksum := string(output)
	if len(checksum) < 64 {
		return "", fmt.Errorf("invalid sha256sum output: %s", checksum)
	}

	return checksum[:64], nil
}
