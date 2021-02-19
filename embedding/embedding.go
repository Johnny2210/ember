package embedding

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/maja42/ember"
	"github.com/maja42/ember/internal"
)

const compatibleVersion = "maja42/ember/v1"

// PrintlnFunc is used for logging the embedding progress.
type PrintlnFunc func(format string, args ...interface{})

// Embed embeds the attachments into the target executable.
//
// out receives the target executable including all attachments.
//
// exe reads from the target executable that should be augmented.
// Embed verifies that the target executable is compatible with this version of ember
// by searching for the magic marker-string (compiled into every executable that imports ember).
// Embed fails if the executable is incompatible or already contains embedded content.
//
// attachments is a map of attachment names to the corresponding readers for the content.
//
// logger (optional) is used to report the progress during embedding.
//
// Note that all ReadSeekers are seeked to their start before usage,
// meaning the entirety of readable content is embedded. Use io.SectionReader to avoid this.
func Embed(out io.Writer, attachments map[string]io.ReadSeeker, exePath string, offset int64, logger PrintlnFunc) error {
	if logger == nil {
		logger = func(string, ...interface{}) {}
	}

	toc, err := buildTOC(attachments)
	if err != nil {
		return fmt.Errorf("build TOC: %w", err)
	}
	jsonTOC, err := json.Marshal(toc)
	if err != nil {
		return fmt.Errorf("marshal TOC: %w", err)
	}

	logger("Writing executable")
	exe, err := os.Open(exePath)
	if err != nil {
		return err
	}

	// write the executable depending on the boundary offset
	// if offset > 0 there are already embedded resources, which means we write
	// the executable until the boundary and append the new+old attachments
	log.Println(offset)
	if offset <= 0 {
		if _, err := io.Copy(out, exe); err != nil {
			return fmt.Errorf("error writing executable: %w", err)
		}
	} else {
		section := io.NewSectionReader(exe, io.SeekStart, offset)
		if _, err := io.Copy(out, section); err != nil {
			return fmt.Errorf("error writing executable: %w", err)
		}
	}

	// Boundary
	if err := internal.WriteBoundary(out); err != nil {
		return err
	}
	// TOC
	logger("Adding TOC (%d bytes)", len(jsonTOC))
	if _, err := out.Write(jsonTOC); err != nil {
		return fmt.Errorf("write TOC: %w", err)
	}
	// Boundary
	if err := internal.WriteBoundary(out); err != nil {
		return err
	}
	// Attachments
	for _, att := range toc {
		logger("Adding %q (%d bytes)", att.Name, att.Size)
		if _, err := io.Copy(out, attachments[att.Name]); err != nil {
			return fmt.Errorf("write attachment %q: %w", att.Name, err)
		}
	}
	// Boundary
	if err := internal.WriteBoundary(out); err != nil {
		return err
	}
	return nil
}

// EmbedFiles embeds the given files into the target executable.
//
// attachments is a map of attachment names to the respective file's filepath.
//
// See Embed for more information.
func EmbedFiles(out io.Writer, exe io.ReadSeeker, attachments map[string]string, exePath string, logger PrintlnFunc) error {
	reader := make(map[string]io.ReadSeeker)
	var offset int64

	if err := verifyTargetExe(exe); err != nil {
		if err != ErrAlreadyEmbedded {
			return fmt.Errorf("verify executable: %w", err)
		}
		log.Printf("executable already contains embedded resources, trying to append")

		offset = internal.SeekBeforeBoundary(exe)

		oldAttachments, err := SaveEmbeddedResources(exePath)
		if err != nil {
			return fmt.Errorf("error saving already embedded resources: %v", err)
		}

		for name, path := range oldAttachments {
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open attachment %q (%q): %w", name, path, err)
			}
			defer file.Close()
			reader[name] = file
		}
	}

	for name, path := range attachments {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open attachment %q (%q): %w", name, path, err)
		}
		//goland:noinspection ALL
		defer file.Close()
		reader[name] = file
	}

	return Embed(out, reader, exePath, offset, logger)
}

// verifyTargetExe ensures that the target executable is compatible.
// The reader is seeked to the beginning afterwards.
// Returns the offset of the
func verifyTargetExe(exe io.ReadSeeker) error {
	// Check if the target executable is compatible.
	// Compatible executables are importing 'ember' in the correct version,
	// causing a marker-string to be present in the binary.
	// String-replace is used to ensure the marker is not present in the embedder-executable.
	marker := "~~MagicMarker for XXX~~"
	marker = strings.ReplaceAll(marker, "XXX", compatibleVersion)
	return VerifyCompatibility(exe, marker)
}

// buildTOC returns the TOC (table-of-contents) for embedding the given data.
// All attachments are seeked to the beginning afterwards.
func buildTOC(attachments map[string]io.ReadSeeker) (internal.TOC, error) {
	toc := make(internal.TOC, 0, len(attachments))

	for name, r := range attachments {
		log.Printf("building Toc entry for: %s", name)
		size, err := getSize(r)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", name, err)
		}
		toc = append(toc, internal.Attachment{
			Name: name,
			Size: size,
		})
	}
	return toc, nil
}

// getSize returns the size of the readable content.
// The reader is seeked to the beginning afterwards.
func getSize(r io.ReadSeeker) (int64, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

// ErrAlreadyEmbedded is returned if the target executable already contains attachments.
var ErrAlreadyEmbedded = errors.New("already contains embedded content")

// VerifyCompatibility ensures that the target executable is compatible and not already augmented.
// This means that the target executable contains the magic-string "marker" that is compiled into the executable,
// which can be easily done by defining it in a global variable and using it in the init() function to ensure that
// it is not optimized away by the go linker. An example can be seen in maja42/ember/marker.go
//   (Note that the calling function's application should build this marker programmatically.
//    Otherwise it will end up in the embeder's executable as well, letting it appear compatible.)
// Returns ErrAlreadyEmbedded if the target executable already contains attachments.
// The reader is seeked to the beginning afterwards.
func VerifyCompatibility(exe io.ReadSeeker, marker string) error {
	// Rewind seeker to start-of-executable (just in case)
	if _, err := exe.Seek(0, io.SeekStart); err != nil {
		return err
	}

	offset := internal.SeekPattern(exe, []byte(marker))
	if offset == -1 { // not a go executable, or does not import correct library(-version) and therefore not the correct marker
		return errors.New("incompatible (magic string not found)")
	}

	offset = internal.SeekBoundary(exe)
	if offset != -1 {
		if _, err := exe.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return ErrAlreadyEmbedded
	}

	if _, err := exe.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return nil
}

// SaveEmbeddedResources saves the already embedded resources temporarily
func SaveEmbeddedResources(exePath string) (map[string]string, error) {
	res := make(map[string]string)
	attachments, err := ember.OpenExe(exePath)
	if err != nil {
		return res, err
	}
	defer attachments.Close()

	contents := attachments.List()
	tmpDir, err := ioutil.TempDir("", "")

	for _, name := range contents {
		r := attachments.Reader(name)
		buf, err := ioutil.ReadAll(r)
		if err != nil {
			return res, err
		}

		filePath := filepath.Join(tmpDir, name)
		err = ioutil.WriteFile(filePath, buf, 0644)
		if err != nil {
			return res, err
		}

		res[name] = filePath
	}
	return res, nil
}
