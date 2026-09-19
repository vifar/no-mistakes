package intent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OmpReaderName is the agent name used in cache keys and DB rows.
const OmpReaderName = "omp"

// ompReader reads Omp sessions from Omp's standard session store. Omp uses the
// same JSONL event protocol as Pi, so parsing is shared with reader_pi.go; the
// storage root and reader identity remain harness-specific.
type ompReader struct{}

// NewOmpReader returns a Reader for Omp coding-agent transcripts.
func NewOmpReader() Reader { return &ompReader{} }

func (r *ompReader) Name() string { return OmpReaderName }

func ompSessionsRoot(home string, useEnv bool) string {
	if useEnv {
		if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
			return filepath.Join(dir, "sessions")
		}
	}
	return filepath.Join(home, ".omp", "agent", "sessions")
}

func (r *ompReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	home, err := resolveHome(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	root := ompSessionsRoot(home, opts.HomeDir == "")
	repoDirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read omp sessions: %w", err)
	}

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	var out []*Session
	for _, repoDir := range repoDirs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !repoDir.IsDir() {
			continue
		}
		dirPath := filepath.Join(root, repoDir.Name())
		files, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			modTime := info.ModTime()
			if !opts.WindowStart.IsZero() && modTime.Before(opts.WindowStart) {
				continue
			}
			if !opts.WindowEnd.IsZero() && modTime.After(opts.WindowEnd.Add(time.Hour)) {
				continue
			}
			path := filepath.Join(dirPath, f.Name())
			meta, err := piPeekMetadata(path)
			if err != nil || meta == nil {
				continue
			}
			if !matcher.matches(ctx, meta.cwd) {
				continue
			}
			sessionID := meta.id
			if sessionID == "" {
				sessionID = strings.TrimSuffix(f.Name(), ".jsonl")
			}
			session := &Session{
				AgentName:     OmpReaderName,
				SessionID:     sessionID,
				CWD:           meta.cwd,
				StartedAt:     meta.startedAt,
				LastActivity:  modTime,
				LastMsgKey:    path + "|" + modTime.UTC().Format(time.RFC3339Nano),
				startedAtPath: path,
			}
			out = append(out, session)
		}
	}
	return out, nil
}

func (r *ompReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("omp: session has no path")
	}
	f, err := os.Open(s.startedAtPath)
	if err != nil {
		return fmt.Errorf("omp open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), piScannerMaxTokenSize)
	var lastID string
	parsedMessages := 0
	seen := make(map[string]struct{})
	seenLive := make(map[string]struct{})
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msgs, id, aggregate, ok := parsePiRecord(line)
		if !ok {
			continue
		}
		if id != "" {
			lastID = id
		}
		priorSeen := seen
		if aggregate {
			priorSeen = make(map[string]struct{}, len(seen))
			for key := range seen {
				priorSeen[key] = struct{}{}
			}
		}
		for _, msg := range msgs {
			if !aggregate && msg.identity != "" {
				if _, ok := seenLive[msg.identity]; ok {
					continue
				}
				seenLive[msg.identity] = struct{}{}
			}
			key := piMessageKey(msg.Message)
			if aggregate {
				if _, ok := priorSeen[key]; ok {
					continue
				}
			}
			seen[key] = struct{}{}
			s.Messages = append(s.Messages, msg.Message)
			parsedMessages++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("omp scan: %w", err)
	}
	if parsedMessages == 0 {
		return fmt.Errorf("omp: session contains no parseable messages")
	}
	if lastID != "" {
		s.LastMsgKey = lastID
	}
	return nil
}
