// Package ircclient parses and downloads CTCP DCC SEND files advertised
// by the irchighway.net #ebooks bots.
//
// Adapted from openbooks/dcc (MIT, see NOTICE).
package ircclient

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"time"
)

// dccReadTimeout bounds how long we'll wait for the next chunk of bytes
// before deciding the bot has stalled. Reset on every successful read.
const dccReadTimeout = 60 * time.Second

var (
	ErrInvalidDCCString = errors.New("invalid dcc send string")
	ErrInvalidIP        = errors.New("unable to convert int IP to string")
	ErrMissingBytes     = errors.New("download size didn't match dcc file size")
)

var dccRegex = regexp.MustCompile(`DCC SEND "?(.+[^"])"?\s(\d+)\s+(\d+)\s+(\d+)\s*`)

// DCCSend describes a remote file the bot is offering over a TCP socket.
type DCCSend struct {
	Filename string
	IP       string
	Port     string
	Size     int64
}

// ParseDCCSend extracts the filename/host/port/size from a CTCP "DCC SEND"
// payload. The IP arrives as a 32-bit integer encoded in decimal.
func ParseDCCSend(text string) (*DCCSend, error) {
	groups := dccRegex.FindStringSubmatch(text)
	if len(groups) == 0 {
		return nil, ErrInvalidDCCString
	}

	ip, err := intToIP(groups[2])
	if err != nil {
		return nil, err
	}

	size, err := strconv.ParseInt(groups[4], 10, 64)
	if err != nil {
		return nil, err
	}

	return &DCCSend{
		Filename: groups[1],
		IP:       ip,
		Port:     groups[3],
		Size:     size,
	}, nil
}

// Download dials the advertised host:port and copies Size bytes into w.
//
// io.Copy is avoided because the DCC server doesn't reliably half-close
// the socket; reading until we hit Size is the only deterministic signal.
// A per-read deadline guards against bots that connect then stall.
func (d DCCSend) Download(w io.Writer) error {
	conn, err := net.DialTimeout("tcp", d.IP+":"+d.Port, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	var received int64
	buf := make([]byte, 4096)
	for received < d.Size {
		if err := conn.SetReadDeadline(time.Now().Add(dccReadTimeout)); err != nil {
			return err
		}
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			received += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	if received != d.Size {
		return ErrMissingBytes
	}
	return nil
}

func intToIP(s string) (string, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return "", ErrInvalidIP
	}
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, uint32(v))
	return ip.String(), nil
}
