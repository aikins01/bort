package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

func writeOutput(stdout io.Writer, outputPath string, write func(io.Writer) error) error {
	if outputPath == "" || outputPath == "-" {
		return write(stdout)
	}

	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	defer file.Close()

	return write(file)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func checkOutputFormat(command, format string) error {
	switch format {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("unsupported %s format %q", command, format)
	}
}

func writeFormattedOutput[T any](stdout io.Writer, outputPath, format string, result T, writeText func(io.Writer, T)) error {
	return writeOutput(stdout, outputPath, func(out io.Writer) error {
		switch format {
		case "text":
			writeText(out, result)
			return nil
		case "json":
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			return encoder.Encode(result)
		default:
			return fmt.Errorf("unsupported format %q", format)
		}
	})
}
