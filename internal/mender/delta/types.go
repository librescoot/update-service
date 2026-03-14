package delta

import "log"

type DeltaMetadata struct {
	NewPayloadChecksum string                `json:"new_payload_checksum"`
	Version            int                   `json:"version"`
	Changes            map[string]ChangeInfo `json:"changes"`
}

type ChangeInfo struct {
	Type    string   `json:"type"` // "new", "modified", "deleted"
	Patch   string   `json:"patch"`
	OldMeta FileMeta `json:"old_meta"`
	NewMeta FileMeta `json:"new_meta"`
}

type FileMeta struct {
	OriginalName       string `json:"original_name"`
	Compressed         bool   `json:"compressed"`
	SHA256             string `json:"sha256"`
	DecompressedSHA256 string `json:"decompressed_sha256"`
}

type ProgressCallback func(percent int, message string)

type Applier struct {
	logger  *log.Logger
	tempDir string
}

func NewApplier(logger *log.Logger, tempDir string) *Applier {
	return &Applier{
		logger:  logger,
		tempDir: tempDir,
	}
}
