package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Segment struct {
	Category string     `json:"category"`
	Segment  [2]float64 `json:"segment"`
	UUID     string     `json:"UUID"`
}

type PlaylistEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Interval struct {
	Start float64
	End   float64
}

func getSponsorBlockSegments(videoID string, categories []string) ([]Segment, error) {
	catJSON, _ := json.Marshal(categories)
	apiURL := fmt.Sprintf("https://sponsor.ajay.app/api/skipSegments?videoID=%s&categories=%s",
		videoID, url.QueryEscape(string(catJSON)))

	resp, err := http.Get(apiURL) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil // No segments found
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sponsorblock returned %d", resp.StatusCode)
	}

	var segments []Segment
	if err := json.NewDecoder(resp.Body).Decode(&segments); err != nil {
		return nil, err
	}
	return segments, nil
}

func ytdlpArgs(base []string, cookiesFile string) []string {
	if cookiesFile != "" {
		base = append(base, "--cookies", cookiesFile)
	}
	return base
}

func getPlaylistEntries(playlistURL, cookiesFile string) ([]PlaylistEntry, error) {
	args := ytdlpArgs([]string{"--flat-playlist", "-j", "--no-warnings", playlistURL}, cookiesFile)
	cmd := exec.Command("yt-dlp", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var entries []PlaylistEntry
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var entry PlaylistEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil && entry.ID != "" {
			entries = append(entries, entry)
		}
	}
	_ = cmd.Wait()
	return entries, nil
}

func downloadAudio(videoID, outputDir, cookiesFile string) (string, error) {
	outputTemplate := filepath.Join(outputDir, "%(id)s.%(ext)s")
	args := ytdlpArgs([]string{
		"-x", "--audio-format", "mp3",
		"--audio-quality", "0",
		"-o", outputTemplate,
		"--no-playlist",
		"--no-warnings",
		fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID),
	}, cookiesFile)
	cmd := exec.Command("yt-dlp", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	pattern := filepath.Join(outputDir, videoID+".mp3")
	if _, err := os.Stat(pattern); err == nil {
		return pattern, nil
	}
	// yt-dlp may use a different extension temporarily; glob for the id
	matches, err := filepath.Glob(filepath.Join(outputDir, videoID+".*"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("downloaded file not found for %s", videoID)
	}
	return matches[0], nil
}

func mergeIntervals(intervals []Interval) []Interval {
	if len(intervals) == 0 {
		return nil
	}
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].Start < intervals[j].Start
	})
	merged := []Interval{intervals[0]}
	for _, iv := range intervals[1:] {
		last := &merged[len(merged)-1]
		if iv.Start <= last.End {
			if iv.End > last.End {
				last.End = iv.End
			}
		} else {
			merged = append(merged, iv)
		}
	}
	return merged
}

func invertIntervals(removeIntervals []Interval, duration float64) []Interval {
	merged := mergeIntervals(removeIntervals)
	var keep []Interval
	pos := 0.0
	for _, iv := range merged {
		if iv.Start > pos+0.01 { // skip tiny gaps
			keep = append(keep, Interval{Start: pos, End: iv.Start})
		}
		pos = iv.End
	}
	if duration-pos > 0.01 {
		keep = append(keep, Interval{Start: pos, End: duration})
	}
	return keep
}

func getAudioDuration(filePath string) (float64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		filePath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var result struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(result.Format.Duration, 64)
}

func cutSegments(inputFile string, segments []Segment, outputFile string) error {
	duration, err := getAudioDuration(inputFile)
	if err != nil {
		return fmt.Errorf("failed to get duration: %w", err)
	}

	var removeIntervals []Interval
	for _, seg := range segments {
		removeIntervals = append(removeIntervals, Interval{
			Start: seg.Segment[0],
			End:   seg.Segment[1],
		})
	}

	keepIntervals := invertIntervals(removeIntervals, duration)
	if len(keepIntervals) == 0 {
		return fmt.Errorf("no content to keep after removing segments")
	}
	// Nothing to remove: just rename
	if len(keepIntervals) == 1 && keepIntervals[0].Start == 0 && keepIntervals[0].End == duration {
		return os.Rename(inputFile, outputFile)
	}

	// Build an ffmpeg filter_complex to keep only the desired intervals
	var parts []string
	for i, iv := range keepIntervals {
		parts = append(parts,
			fmt.Sprintf("[0:a]atrim=start=%.6f:end=%.6f,asetpts=PTS-STARTPTS[a%d]", iv.Start, iv.End, i))
	}
	var inputs strings.Builder
	for i := range keepIntervals {
		fmt.Fprintf(&inputs, "[a%d]", i)
	}
	parts = append(parts,
		fmt.Sprintf("%sconcat=n=%d:v=0:a=1[out]", inputs.String(), len(keepIntervals)))

	filter := strings.Join(parts, ";")

	cmd := exec.Command("ffmpeg", "-y",
		"-i", inputFile,
		"-filter_complex", filter,
		"-map", "[out]",
		outputFile,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	playlistURL := flag.String("url", "", "YouTube playlist or video URL")
	outputDir := flag.String("output", "./music", "Output directory")
	categories := flag.String("categories",
		"sponsor,outro,selfpromo,interaction,music_offtopic",
		"Comma-separated SponsorBlock categories to remove")
	cookiesFile := flag.String("cookies", "", "Path to cookies.txt file (required for private playlists)")
	flag.Parse()

	if *playlistURL == "" {
		fmt.Fprintln(os.Stderr, "Usage: autodl-music -url <playlist-url> [-output <dir>] [-categories <cat1,cat2>] [-cookies <cookies.txt>]")
		fmt.Fprintln(os.Stderr, "\nAvailable categories: sponsor, intro, outro, selfpromo, interaction, music_offtopic, preview, filler")
		fmt.Fprintln(os.Stderr, "\nFor private playlists, export cookies from your browser and pass them with -cookies.")
		os.Exit(1)
	}

	if *cookiesFile != "" {
		if _, err := os.Stat(*cookiesFile); err != nil {
			fmt.Fprintf(os.Stderr, "Cookies file not found: %s\n", *cookiesFile)
			os.Exit(1)
		}
	}

	cats := strings.Split(*categories, ",")
	for i, c := range cats {
		cats[i] = strings.TrimSpace(c)
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create output dir: %v\n", err)
		os.Exit(1)
	}

	tmpDir := filepath.Join(*outputDir, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create tmp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Println("Fetching playlist entries...")
	entries, err := getPlaylistEntries(*playlistURL, *cookiesFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get playlist: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Found %d video(s)\n", len(entries))

	var failed []string
	for i, entry := range entries {
		fmt.Printf("\n[%d/%d] %s (%s)\n", i+1, len(entries), entry.Title, entry.ID)

		finalPath := filepath.Join(*outputDir, entry.ID+".mp3")
		if _, err := os.Stat(finalPath); err == nil {
			fmt.Println("  Already processed, skipping.")
			continue
		}

		segments, err := getSponsorBlockSegments(entry.ID, cats)
		if err != nil {
			fmt.Printf("  Warning: SponsorBlock error: %v\n", err)
		}
		fmt.Printf("  SponsorBlock: %d segment(s) to remove\n", len(segments))

		downloadedFile, err := downloadAudio(entry.ID, tmpDir, *cookiesFile)
		if err != nil {
			fmt.Printf("  Error downloading: %v\n", err)
			failed = append(failed, entry.ID)
			continue
		}

		if len(segments) > 0 {
			fmt.Println("  Cutting segments...")
			if err := cutSegments(downloadedFile, segments, finalPath); err != nil {
				fmt.Printf("  Error cutting segments: %v — saving uncut version\n", err)
				if err2 := os.Rename(downloadedFile, finalPath); err2 != nil {
					fmt.Printf("  Error saving file: %v\n", err2)
					failed = append(failed, entry.ID)
					continue
				}
			} else {
				os.Remove(downloadedFile)
			}
		} else {
			if err := os.Rename(downloadedFile, finalPath); err != nil {
				fmt.Printf("  Error saving file: %v\n", err)
				failed = append(failed, entry.ID)
				continue
			}
		}
		fmt.Printf("  Saved: %s\n", finalPath)
	}

	fmt.Printf("\nDone. %d/%d succeeded.\n", len(entries)-len(failed), len(entries))
	if len(failed) > 0 {
		fmt.Printf("Failed: %s\n", strings.Join(failed, ", "))
		os.Exit(1)
	}
}
