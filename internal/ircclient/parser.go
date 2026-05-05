// Search-result text parser for the irchighway.net SearchBot reply format.
//
// Adapted from openbooks/core/search_parser.go (MIT, see NOTICE).
package ircclient

import (
	"bufio"
	"errors"
	"io"
	"sort"
	"strings"
)

// fileTypes the SearchBot is known to advertise. Archive extensions
// (rar, zip) MUST stay last — see getTitle for why.
var fileTypes = [...]string{
	"epub", "mobi", "azw3", "html", "rtf", "pdf", "cdr", "lit",
	"cbr", "doc", "htm", "jpg", "txt",
	"rar", "zip",
}

// BookResult is one row from the SearchBot results file.
type BookResult struct {
	Server string
	Author string
	Title  string
	Format string
	Size   string
	Full   string // verbatim "!Bot Author - Title.epub" — what we send back to download
}

// ParseError captures lines we couldn't classify so the user can still
// see what came back.
type ParseError struct {
	Line string
	Err  error
}

// ParseSearch consumes the SearchBot's results .txt and returns one
// BookResult per "!Bot ..." line.
func ParseSearch(r io.Reader) ([]BookResult, []ParseError) {
	books := make([]BookResult, 0, 64)
	errs := make([]ParseError, 0)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "!") {
			continue
		}
		b, err := parseLine(line)
		if err != nil {
			errs = append(errs, ParseError{Line: line, Err: err})
			continue
		}
		books = append(books, b)
	}

	sort.Slice(books, func(i, j int) bool { return books[i].Server < books[j].Server })
	return books, errs
}

func parseLine(line string) (BookResult, error) {
	server, err := getServer(line)
	if err != nil {
		return BookResult{}, err
	}

	author, err := getAuthor(line)
	if err != nil {
		return BookResult{}, err
	}

	title, format, titleEnd := getTitle(line)
	if titleEnd == -1 {
		return BookResult{}, errors.New("could not parse title")
	}

	size, sizeEnd := getSize(line)

	return BookResult{
		Server: server,
		Author: author,
		Title:  title,
		Format: format,
		Size:   size,
		Full:   strings.TrimSpace(line[:sizeEnd]),
	}, nil
}

func getServer(line string) (string, error) {
	if line[0] != '!' {
		return "", errors.New("result lines must start with '!'")
	}
	sp := strings.Index(line, " ")
	if sp == -1 {
		return "", errors.New("could not parse server")
	}
	return line[1:sp], nil
}

func getAuthor(line string) (string, error) {
	sp := strings.Index(line, " ")
	dash := strings.Index(line, " - ")
	if dash == -1 {
		return "", errors.New("could not parse author")
	}
	author := line[sp+1 : dash]

	// Some servers prefix authors with a hex hash like "%F77FE9FF1CCD% ".
	if strings.Contains(author, "%") {
		parts := strings.SplitAfterN(author, " ", 2)
		if len(parts) == 2 {
			return parts[1], nil
		}
	}
	return author, nil
}

// getTitle returns the title text, the matching file extension, and the
// byte index in `line` of the period before the extension. If no known
// extension is found, endIndex is -1.
func getTitle(line string) (title, format string, endIndex int) {
	endIndex = -1
	for _, ext := range fileTypes {
		idx := strings.Index(line, "."+ext)
		if idx == -1 {
			continue
		}
		format = ext
		// .rar/.zip wrappers usually mention the real format in the
		// title (e.g. "Foo (epub).rar"). Re-scan for the inner format.
		if ext == "rar" || ext == "zip" {
			for _, inner := range fileTypes[:len(fileTypes)-2] {
				if strings.Contains(strings.ToLower(line[:idx]), inner) {
					format = inner
				}
			}
		}
		dash := strings.Index(line, " - ")
		title = line[dash+len(" - ") : idx]
		endIndex = idx
	}
	return
}

func getSize(line string) (string, int) {
	const delim = " ::INFO:: "
	idx := strings.LastIndex(line, delim)
	if idx == -1 {
		return "N/A", len(line)
	}
	parts := strings.Split(line[idx+len(delim):], " ")
	return parts[0], idx
}
