package api

// The external tests compare the console's reading of a command line, and
// the commands and flags it singles out, with mp's real command tree (which
// imports this package, so only an api_test package can build it).
var (
	ConsoleCommand    = consoleCommand
	ConsoleOwnFlags   = consoleOwnFlags
	ConsoleLocalFiles = consoleLocalFiles
	ConsoleRootOnly   = consoleRootOnly
	ConsoleStdinTwins = consoleStdinTwins
)
