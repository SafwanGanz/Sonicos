package plugins

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

type ttsVoice struct {
	ShortName string
	Alias     string
	Gender    string
	Locale    string
	Lang      string
}

type ttsRegistry struct {
	mu        sync.RWMutex
	voices    []ttsVoice
	byAlias   map[string]*ttsVoice
	byShort   map[string]*ttsVoice
	loadedAt  time.Time
	loadError error
	loaded    bool
}

var ttsReg = &ttsRegistry{}

func (r *ttsRegistry) voices24h(ctx context.Context) ([]ttsVoice, error) {
	r.mu.RLock()
	if r.loaded && time.Since(r.loadedAt) < 24*time.Hour {
		v := r.voices
		err := r.loadError
		r.mu.RUnlock()
		return v, err
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && time.Since(r.loadedAt) < 24*time.Hour {
		return r.voices, r.loadError
	}

	voices, err := loadVoicesFromCLI(ctx)
	if err != nil {

		return r.voices, err
	}
	r.voices = voices
	r.loadError = nil
	r.loadedAt = time.Now()
	r.loaded = true
	r.byAlias, r.byShort = buildLookups(voices)
	return voices, nil
}

func (r *ttsRegistry) find(ctx context.Context, name string) (*ttsVoice, bool) {
	r.voices24h(ctx)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		if v, ok := r.byShort["en-us-arianeural"]; ok {
			return v, true
		}
		if v, ok := r.byAlias["aria"]; ok {
			return v, true
		}
		if len(r.voices) > 0 {
			return &r.voices[0], true
		}
		return nil, false
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if v, ok := r.byAlias[key]; ok {
		return v, true
	}
	if v, ok := r.byShort[key]; ok {
		return v, true
	}
	return nil, false
}

func loadVoicesFromCLI(ctx context.Context) ([]ttsVoice, error) {
	bin := ttsSynthBinary()
	cmd := exec.CommandContext(ctx, bin, "--list-voices")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Error("tts: failed to load voice list", "error", err, "stderr", strings.TrimSpace(stderr.String()))
		return nil, fmt.Errorf("voice service unavailable")
	}
	return parseVoiceTable(stdout.String()), nil
}

func parseVoiceTable(output string) []ttsVoice {
	lines := strings.Split(output, "\n")
	var raw []ttsVoice
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		shortName := fields[0]
		gender := fields[1]
		if !looksLikeVoiceShortName(shortName) {
			continue
		}
		if gender != "Female" && gender != "Male" {
			continue
		}
		locale := extractLocale(shortName)
		lang := extractLang(locale)
		raw = append(raw, ttsVoice{
			ShortName: shortName,
			Gender:    gender,
			Locale:    locale,
			Lang:      lang,
		})
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].ShortName < raw[j].ShortName })
	assignAliases(raw)
	return raw
}

func looksLikeVoiceShortName(s string) bool {
	parts := strings.SplitN(s, "-", 3)
	if len(parts) < 3 {
		return false
	}
	for _, c := range parts[0] {
		if !unicode.IsLetter(c) {
			return false
		}
	}
	if len(parts[0]) < 2 || len(parts[0]) > 3 {
		return false
	}
	if len(parts[1]) < 2 {
		return false
	}
	return len(parts[2]) > 0
}

func extractLocale(shortName string) string {
	parts := strings.SplitN(shortName, "-", 3)
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return shortName
}

func extractLang(locale string) string {
	if idx := strings.Index(locale, "-"); idx > 0 {
		return locale[:idx]
	}
	return locale
}

func extractPersona(shortName string) string {
	parts := strings.SplitN(shortName, "-", 3)
	if len(parts) < 3 {
		return strings.ToLower(shortName)
	}
	p := parts[2]
	for _, suffix := range []string{"MultilingualNeuralHD", "MultilingualNeural", "NeuralHD", "Neural", "Online"} {
		if strings.HasSuffix(p, suffix) {
			p = p[:len(p)-len(suffix)]
			break
		}
	}
	p = strings.TrimRight(p, "-")
	return strings.ToLower(p)
}

func assignAliases(voices []ttsVoice) {
	personaCount := make(map[string]int)
	for _, v := range voices {
		personaCount[extractPersona(v.ShortName)]++
	}
	used := make(map[string]bool)
	for i := range voices {
		persona := extractPersona(voices[i].ShortName)
		if personaCount[persona] == 1 {
			voices[i].Alias = persona
			used[persona] = true
		}
	}
	for i := range voices {
		if voices[i].Alias != "" {
			continue
		}
		persona := extractPersona(voices[i].ShortName)
		region := strings.ToLower(extractRegion(voices[i].Locale))
		candidate := persona + "-" + region
		if used[candidate] {
			candidate = strings.ToLower(voices[i].Locale) + "-" + persona
		}
		voices[i].Alias = candidate
		used[candidate] = true
	}
}

func extractRegion(locale string) string {
	parts := strings.SplitN(locale, "-", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return locale
}

func buildLookups(voices []ttsVoice) (byAlias, byShort map[string]*ttsVoice) {
	byAlias = make(map[string]*ttsVoice, len(voices))
	byShort = make(map[string]*ttsVoice, len(voices))
	for i := range voices {
		v := &voices[i]
		byAlias[v.Alias] = v
		byShort[strings.ToLower(v.ShortName)] = v
	}
	return
}

func init() {
	command.Register(command.Command{
		Name:        "tts",
		Aliases:     []string{"say", "speak"},
		Category:    "Media & Stickers",
		Description: "Text-to-speech with Sonicos voices. !tts <text> [voice] | !tts list [male|female|lang] [page]",
		Handler:     ttsCmd,
	})
}

func ttsCmd(ctx context.Context, _ command.Message, args []string, reply command.ReplyFunc) error {
	prefix := botPrefix()

	if len(args) == 0 {
		return reply(ctx, command.Reply{
			Text: fmt.Sprintf(
				"🔊 *Sonicos Voice*\n"+
					"Usage: `%[1]stts <text> [voice]`\n"+
					"Browse voices: `%[1]stts list` | `%[1]stts list male` | `%[1]stts list female`\n"+
					"Filter by language: `%[1]stts list en` | `%[1]stts list zh` | `%[1]stts list hi`\n"+
					"Example: `%[1]stts Hello world aria` | `%[1]stts Bonjour henri`",
				prefix,
			),
		})
	}

	sub := strings.ToLower(strings.TrimSpace(args[0]))

	if sub == "list" || sub == "voices" {
		filter := ""
		if len(args) > 1 {
			filter = strings.ToLower(strings.TrimSpace(strings.Join(args[1:], " ")))
		}
		return ttsListVoices(ctx, filter, reply, prefix)
	}

	if sub == "reload" {
		ttsReg.mu.Lock()
		ttsReg.loaded = false
		ttsReg.mu.Unlock()
		if _, err := ttsReg.voices24h(ctx); err != nil {
			return reply(ctx, command.Reply{Text: "❌ Voice service is currently unavailable. Try again later."})
		}
		ttsReg.mu.RLock()
		count := len(ttsReg.voices)
		ttsReg.mu.RUnlock()
		return reply(ctx, command.Reply{Text: fmt.Sprintf("✅ Reloaded %d voices.", count)})
	}

	voice, found := ttsReg.find(ctx, args[len(args)-1])
	var text string
	if found && len(args) > 1 {
		text = strings.Join(args[:len(args)-1], " ")
	} else {
		voice, _ = ttsReg.find(ctx, "")
		text = strings.Join(args, " ")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: `%stts <text> [voice]`", prefix)})
	}
	if voice == nil {
		return reply(ctx, command.Reply{Text: "❌ Voice service is currently unavailable. Try again later."})
	}

	audioData, err := synthesizeTTS(ctx, text, voice.ShortName)
	if err != nil {
		slog.Error("tts: synthesis failed", "voice", voice.ShortName, "error", err)
		return reply(ctx, command.Reply{Text: "❌ Could not generate speech. The voice service may be unreachable — try again in a moment."})
	}

	opusData, seconds, waveform, err := mp3ToOpusPTT(ctx, audioData)
	if err != nil {
		slog.Error("tts: audio encode failed", "error", err)
		return reply(ctx, command.Reply{Text: "❌ Could not process audio. Make sure ffmpeg is installed on the server."})
	}

	return reply(ctx, command.Reply{
		Audio: &command.AudioReply{
			Data:     opusData,
			MIMEType: "audio/ogg; codecs=opus",
			Seconds:  seconds,
			Waveform: waveform,
			PTT:      true,
		},
	})
}

const ttsVoicesPerPage = 20

func ttsListVoices(ctx context.Context, filter string, reply command.ReplyFunc, prefix string) error {
	voices, err := ttsReg.voices24h(ctx)
	if err != nil {
		return reply(ctx, command.Reply{Text: "❌ Voice list is currently unavailable. Try again later."})
	}

	args := strings.Fields(filter)
	genderFilter := ""
	langFilter := ""
	page := 1

	if len(args) >= 1 {
		switch strings.ToLower(args[0]) {
		case "female", "f":
			genderFilter = "Female"
		case "male", "m":
			genderFilter = "Male"
		default:
			if p, err := strconv.Atoi(args[0]); err == nil && p > 0 && len(args) == 1 {
				page = p
			} else {
				langFilter = strings.ToLower(args[0])
			}
		}
	}
	if len(args) >= 2 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p
		}
	}

	var females, males []ttsVoice
	for _, v := range voices {
		if langFilter != "" {
			localeLow := strings.ToLower(v.Locale)
			langLow := strings.ToLower(v.Lang)
			if langLow != langFilter && localeLow != langFilter && !strings.HasPrefix(localeLow, langFilter+"-") {
				continue
			}
		}
		switch strings.ToLower(v.Gender) {
		case "female":
			if genderFilter == "" || genderFilter == "Female" {
				females = append(females, v)
			}
		case "male":
			if genderFilter == "" || genderFilter == "Male" {
				males = append(males, v)
			}
		}
	}

	total := len(females) + len(males)
	if total == 0 {
		return reply(ctx, command.Reply{
			Text: fmt.Sprintf("No voices found for *%s*.\nTry `%stts list en` or `%stts list` to browse all voices.", filter, prefix, prefix),
		})
	}

	sort.Slice(females, func(i, j int) bool { return females[i].Alias < females[j].Alias })
	sort.Slice(males, func(i, j int) bool { return males[i].Alias < males[j].Alias })

	var selectedVoices []ttsVoice
	var groupLabel string
	var groupIcon string

	if genderFilter == "Female" {
		selectedVoices = females
		groupLabel = "FEMALE"
		groupIcon = "👩"
	} else if genderFilter == "Male" {
		selectedVoices = males
		groupLabel = "MALE"
		groupIcon = "👨"
	} else {
		selectedVoices = append(females, males...)
		groupLabel = "ALL"
		groupIcon = "🎙️"
	}

	totalPages := (len(selectedVoices) + ttsVoicesPerPage - 1) / ttsVoicesPerPage
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * ttsVoicesPerPage
	end := start + ttsVoicesPerPage
	if end > len(selectedVoices) {
		end = len(selectedVoices)
	}
	pageVoices := selectedVoices[start:end]

	var chunks []string
	var sb strings.Builder

	header := fmt.Sprintf("%s *Sonicos Voice — %s (%d voices, page %d/%d)*\n`%stts <text> <alias>`\n", groupIcon, groupLabel, len(selectedVoices), page, totalPages, prefix)
	if langFilter != "" {
		header = fmt.Sprintf("🎙️ *Sonicos Voice — %s (%d voices, page %d/%d)*\n`%stts <text> <alias>`\n", strings.ToUpper(langFilter), len(selectedVoices), page, totalPages, prefix)
	}
	sb.WriteString(header)

	flushIfNeeded := func(section string) {
		if len(sb.String())+len(section) > 3800 {
			chunks = append(chunks, strings.TrimRight(sb.String(), "\n"))
			sb.Reset()
		}
		sb.WriteString(section)
	}

	flushIfNeeded(fmt.Sprintf("\n────────────────────────\n"))
	for _, v := range pageVoices {
		flushIfNeeded(fmt.Sprintf("• `%-16s` %s %s\n", v.Alias, localeFlag(v.Locale), v.Locale))
	}

	var nav strings.Builder
	if totalPages > 1 {
		nav.WriteString(fmt.Sprintf("\n📄 Page %d of %d", page, totalPages))
		if genderFilter != "" {
			nav.WriteString(fmt.Sprintf(" · `%stts list %s %d` for next", prefix, strings.ToLower(genderFilter), page+1))
		} else if langFilter != "" {
			nav.WriteString(fmt.Sprintf(" · `%stts list %s %d` for next", prefix, langFilter, page+1))
		} else {
			nav.WriteString(fmt.Sprintf(" · `%stts list %d` for next", prefix, page+1))
		}
	}
	nav.WriteString(fmt.Sprintf("\n💡 _Default: `aria` · Filter: `%[1]stts list male` · `%[1]stts list female` · `%[1]stts list en`_", prefix))
	flushIfNeeded(nav.String())

	if sb.Len() > 0 {
		chunks = append(chunks, strings.TrimRight(sb.String(), "\n"))
	}

	for _, chunk := range chunks {
		if err := reply(ctx, command.Reply{Text: chunk}); err != nil {
			return err
		}
	}
	return nil
}

func localeFlag(locale string) string {
	flags := map[string]string{
		"af-ZA": "🇿🇦", "am-ET": "🇪🇹", "ar-AE": "🇦🇪", "ar-BH": "🇧🇭",
		"ar-DZ": "🇩🇿", "ar-EG": "🇪🇬", "ar-IQ": "🇮🇶", "ar-JO": "🇯🇴",
		"ar-KW": "🇰🇼", "ar-LB": "🇱🇧", "ar-LY": "🇱🇾", "ar-MA": "🇲🇦",
		"ar-OM": "🇴🇲", "ar-QA": "🇶🇦", "ar-SA": "🇸🇦", "ar-SY": "🇸🇾",
		"ar-TN": "🇹🇳", "ar-YE": "🇾🇪", "az-AZ": "🇦🇿", "bg-BG": "🇧🇬",
		"bn-BD": "🇧🇩", "bn-IN": "🇮🇳", "bs-BA": "🇧🇦", "ca-ES": "🇪🇸",
		"cs-CZ": "🇨🇿", "cy-GB": "🏴󠁧󠁢󠁷󠁬󠁳󠁿", "da-DK": "🇩🇰", "de-AT": "🇦🇹",
		"de-CH": "🇨🇭", "de-DE": "🇩🇪", "el-GR": "🇬🇷", "en-AU": "🇦🇺",
		"en-CA": "🇨🇦", "en-GB": "🇬🇧", "en-HK": "🇭🇰", "en-IE": "🇮🇪",
		"en-IN": "🇮🇳", "en-KE": "🇰🇪", "en-NG": "🇳🇬", "en-NZ": "🇳🇿",
		"en-PH": "🇵🇭", "en-SG": "🇸🇬", "en-TZ": "🇹🇿", "en-US": "🇺🇸",
		"en-ZA": "🇿🇦", "es-AR": "🇦🇷", "es-BO": "🇧🇴", "es-CL": "🇨🇱",
		"es-CO": "🇨🇴", "es-CR": "🇨🇷", "es-CU": "🇨🇺", "es-DO": "🇩🇴",
		"es-EC": "🇪🇨", "es-ES": "🇪🇸", "es-GQ": "🇬🇶", "es-GT": "🇬🇹",
		"es-HN": "🇭🇳", "es-MX": "🇲🇽", "es-NI": "🇳🇮", "es-PA": "🇵🇦",
		"es-PE": "🇵🇪", "es-PR": "🇵🇷", "es-PY": "🇵🇾", "es-SV": "🇸🇻",
		"es-US": "🇺🇸", "es-UY": "🇺🇾", "es-VE": "🇻🇪", "et-EE": "🇪🇪",
		"eu-ES": "🇪🇸", "fa-IR": "🇮🇷", "fi-FI": "🇫🇮", "fil-PH": "🇵🇭",
		"fr-BE": "🇧🇪", "fr-CA": "🇨🇦", "fr-CH": "🇨🇭", "fr-FR": "🇫🇷",
		"ga-IE": "🇮🇪", "gl-ES": "🇪🇸", "gu-IN": "🇮🇳", "he-IL": "🇮🇱",
		"hi-IN": "🇮🇳", "hr-HR": "🇭🇷", "hu-HU": "🇭🇺", "hy-AM": "🇦🇲",
		"id-ID": "🇮🇩", "is-IS": "🇮🇸", "it-IT": "🇮🇹", "ja-JP": "🇯🇵",
		"jv-ID": "🇮🇩", "ka-GE": "🇬🇪", "kk-KZ": "🇰🇿", "km-KH": "🇰🇭",
		"kn-IN": "🇮🇳", "ko-KR": "🇰🇷", "lo-LA": "🇱🇦", "lt-LT": "🇱🇹",
		"lv-LV": "🇱🇻", "mk-MK": "🇲🇰", "ml-IN": "🇮🇳", "mn-MN": "🇲🇳",
		"mr-IN": "🇮🇳", "ms-MY": "🇲🇾", "mt-MT": "🇲🇹", "my-MM": "🇲🇲",
		"nb-NO": "🇳🇴", "ne-NP": "🇳🇵", "nl-BE": "🇧🇪", "nl-NL": "🇳🇱",
		"or-IN": "🇮🇳", "pa-IN": "🇮🇳", "pl-PL": "🇵🇱", "ps-AF": "🇦🇫",
		"pt-BR": "🇧🇷", "pt-PT": "🇵🇹", "ro-RO": "🇷🇴", "ru-RU": "🇷🇺",
		"si-LK": "🇱🇰", "sk-SK": "🇸🇰", "sl-SI": "🇸🇮", "so-SO": "🇸🇴",
		"sq-AL": "🇦🇱", "sr-RS": "🇷🇸", "su-ID": "🇮🇩", "sv-SE": "🇸🇪",
		"sw-KE": "🇰🇪", "sw-TZ": "🇹🇿", "ta-IN": "🇮🇳", "ta-LK": "🇱🇰",
		"ta-MY": "🇲🇾", "ta-SG": "🇸🇬", "te-IN": "🇮🇳", "th-TH": "🇹🇭",
		"tr-TR": "🇹🇷", "uk-UA": "🇺🇦", "ur-IN": "🇮🇳", "ur-PK": "🇵🇰",
		"uz-UZ": "🇺🇿", "vi-VN": "🇻🇳", "zh-CN": "🇨🇳", "zh-HK": "🇭🇰",
		"zh-TW": "🇹🇼", "zu-ZA": "🇿🇦",
	}
	if f, ok := flags[locale]; ok {
		return f
	}
	return "🌐"
}

func synthesizeTTS(ctx context.Context, text, voiceShortName string) ([]byte, error) {
	bin := ttsSynthBinary()
	id := ttsRandID()
	txtFile := filepath.Join(os.TempDir(), fmt.Sprintf("tts_%d.txt", id))
	if err := os.WriteFile(txtFile, []byte(text), 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(txtFile)

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("tts_%d.mp3", id))
	defer os.Remove(tmpFile)

	cmd := exec.CommandContext(ctx, bin,
		"--voice", voiceShortName,
		"--file", txtFile,
		"--write-media", tmpFile,
		"--no-progress",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func mp3ToOpusPTT(ctx context.Context, mp3Data []byte) ([]byte, uint32, []byte, error) {
	ffmpeg := ffmpegBinary()
	inputFile := filepath.Join(os.TempDir(), fmt.Sprintf("tts_in_%d.mp3", ttsRandID()))
	outputFile := filepath.Join(os.TempDir(), fmt.Sprintf("tts_out_%d.ogg", ttsRandID()))

	if err := os.WriteFile(inputFile, mp3Data, 0o600); err != nil {
		return nil, 0, nil, err
	}
	defer os.Remove(inputFile)
	defer os.Remove(outputFile)

	cmd := exec.CommandContext(ctx, ffmpeg,
		"-y", "-i", inputFile,
		"-c:a", "libopus",
		"-ac", "1",
		"-ar", "48000",
		"-b:a", "64k",
		"-application", "voip",
		outputFile,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, 0, nil, fmt.Errorf("ffmpeg: %w", err)
	}

	opusData, err := os.ReadFile(outputFile)
	if err != nil {
		return nil, 0, nil, err
	}

	seconds := probeTTSDuration(ctx, ffmpeg, outputFile)
	waveform := generateTTSWaveform(ctx, ffmpeg, inputFile, 64)
	return opusData, seconds, waveform, nil
}

func probeTTSDuration(ctx context.Context, ffmpeg, path string) uint32 {
	cmd := exec.CommandContext(ctx, ffmpeg, "-i", path)
	var out bytes.Buffer
	cmd.Stderr = &out
	_ = cmd.Run()
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "Duration:") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "Duration:" && i+1 < len(parts) {
					dur := strings.TrimSuffix(strings.TrimSpace(parts[i+1]), ",")
					var h, m int
					var s float64
					fmt.Sscanf(dur, "%d:%d:%f", &h, &m, &s)
					if total := float64(h*3600+m*60) + s; total > 0 {
						return uint32(total + 0.5)
					}
				}
			}
		}
	}
	return 0
}

func generateTTSWaveform(ctx context.Context, ffmpeg, path string, points int) []byte {
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-i", path,
		"-af", "asetnsamples=n=1024,astats=metadata=1:reset=1,ametadata=print:key=lavfi.astats.Overall.RMS_level:file=-",
		"-f", "null", "-",
	)
	var out bytes.Buffer
	cmd.Stderr = &out
	_ = cmd.Run()

	var levels []float64
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "lavfi.astats.Overall.RMS_level=") {
			val := strings.TrimPrefix(line, "lavfi.astats.Overall.RMS_level=")
			var f float64
			if _, err := fmt.Sscanf(val, "%f", &f); err == nil {
				levels = append(levels, f)
			}
		}
	}

	wave := make([]byte, points)
	if len(levels) == 0 {
		for i := range wave {
			wave[i] = 100
		}
		return wave
	}
	minDB, maxDB := -60.0, 0.0
	for _, l := range levels {
		if l < minDB {
			minDB = l
		}
		if l > maxDB {
			maxDB = l
		}
	}
	rng := maxDB - minDB
	if rng == 0 {
		rng = 1
	}
	for i := 0; i < points; i++ {
		idx := int(float64(i) * float64(len(levels)) / float64(points))
		if idx >= len(levels) {
			idx = len(levels) - 1
		}
		wave[i] = byte((levels[idx] - minDB) / rng * 100)
	}
	return wave
}

func ttsSynthBinary() string {
	if v := strings.TrimSpace(os.Getenv("SONICOS_TTS_BIN")); v != "" {
		return v
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "edge-tts"))
	}
	candidates = append(candidates,
		"/usr/local/bin/edge-tts",
		"/usr/bin/edge-tts",
	)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("edge-tts"); err == nil {
		return p
	}
	return "edge-tts"
}

func ffmpegBinary() string {
	for _, env := range []string{"FFMPEG_PATH", "FFMPEG"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return "ffmpeg"
}

var ttsRandCounter int64

func ttsRandID() int64 {
	n := atomic.AddInt64(&ttsRandCounter, 1)
	return n*1_000_000 + int64(os.Getpid())
}
