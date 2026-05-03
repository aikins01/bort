package cli

import "os"

func isInteractiveTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && isTerminalFile(file)
}
