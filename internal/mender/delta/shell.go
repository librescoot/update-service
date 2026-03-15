package delta

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ShellGzip compresses a file using system gzip command
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
	return ShellGunzipTracked(inputFile, outputFile, nil)
}

// ShellGunzipTracked decompresses with byte tracking through the shared tracker.
func ShellGunzipTracked(inputFile, outputFile string, tracker *progressTracker) error {
	outFile, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	cmd := exec.Command("gunzip", "-c", inputFile)
	cmd.Stderr = os.Stderr

	if tracker != nil {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("gunzip stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("gunzip start: %w", err)
		}
		if _, err := io.Copy(outFile, tracker.reader(stdout, "decompressing")); err != nil {
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

	checksum := string(output)
	if len(checksum) < 64 {
		return "", fmt.Errorf("invalid sha256sum output: %s", checksum)
	}

	return checksum[:64], nil
}
