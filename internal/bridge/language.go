package bridge

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Language maps a human name / country / code to the two audio engines:
//   - Whisper: the faster-whisper language code for transcription (the model is
//     multilingual; "" means auto-detect). This is the INCOMING engine.
//   - Piper: the piper voice model for synthesis ("" = no TTS voice for this
//     language, so spoken replies fall back to text). This is the OUTGOING
//     engine, downloaded on demand by scripts/tts.
type Language struct {
	Name    string
	Whisper string
	Piper   string
	Aliases []string // lowercased match keys (language names, countries, codes)
}

// languages is the curated table. Piper voices were picked from the
// rhasspy/piper-voices catalog, preferring well-known medium-quality voices.
// Whisper codes are ISO-639-1 (what faster-whisper expects).
var languages = []Language{
	{"Auto-detect", "", "en_US-amy-medium", []string{"auto", "detect", "default", "off"}},
	{"English (US)", "en", "en_US-amy-medium", []string{"english", "en", "en_us", "us", "usa", "united states", "american", "america"}},
	{"English (UK)", "en", "en_GB-alan-medium", []string{"british", "en_gb", "uk", "united kingdom", "britain", "england", "gb"}},
	{"Spanish (Spain)", "es", "es_ES-davefx-medium", []string{"spanish", "es", "es_es", "spain", "espanol", "español", "españa", "castilian"}},
	{"Spanish (Mexico)", "es", "es_MX-ald-medium", []string{"mexican", "es_mx", "mexico", "méxico", "latam", "latin america", "latin american"}},
	{"French", "fr", "fr_FR-siwis-medium", []string{"french", "fr", "fr_fr", "france", "français", "francais"}},
	{"German", "de", "de_DE-thorsten-medium", []string{"german", "de", "de_de", "germany", "deutsch", "deutschland"}},
	{"Italian", "it", "it_IT-paola-medium", []string{"italian", "it", "it_it", "italy", "italiano", "italia"}},
	{"Portuguese", "pt", "pt_BR-faber-medium", []string{"portuguese", "pt", "pt_br", "brazil", "brasil", "brazilian", "portugal", "português", "portugues"}},
	{"Dutch", "nl", "nl_NL-pim-medium", []string{"dutch", "nl", "nl_nl", "netherlands", "holland", "nederlands"}},
	{"Russian", "ru", "ru_RU-denis-medium", []string{"russian", "ru", "ru_ru", "russia"}},
	{"Polish", "pl", "pl_PL-gosia-medium", []string{"polish", "pl", "pl_pl", "poland", "polski"}},
	{"Ukrainian", "uk", "uk_UA-ukrainian_tts-medium", []string{"ukrainian", "uk", "uk_ua", "ukraine"}},
	{"Turkish", "tr", "tr_TR-dfki-medium", []string{"turkish", "tr", "tr_tr", "turkey", "türkçe", "turkce"}},
	{"Chinese", "zh", "zh_CN-huayan-medium", []string{"chinese", "zh", "zh_cn", "china", "mandarin"}},
	{"Arabic", "ar", "ar_JO-kareem-medium", []string{"arabic", "ar", "ar_jo", "arab"}},
	{"Swedish", "sv", "sv_SE-nst-medium", []string{"swedish", "sv", "sv_se", "sweden", "svenska"}},
	{"Greek", "el", "el_GR-rapunzelina-medium", []string{"greek", "el", "el_gr", "greece"}},
	{"Czech", "cs", "cs_CZ-jirka-medium", []string{"czech", "cs", "cs_cz", "czechia", "čeština"}},
	{"Romanian", "ro", "ro_RO-mihai-medium", []string{"romanian", "ro", "ro_ro", "romania"}},
	{"Hungarian", "hu", "hu_HU-anna-medium", []string{"hungarian", "hu", "hu_hu", "hungary", "magyar"}},
	{"Catalan", "ca", "ca_ES-upc_ona-medium", []string{"catalan", "ca", "ca_es", "catalonia", "català"}},
	{"Hindi", "hi", "hi_IN-pratham-medium", []string{"hindi", "hi", "hi_in", "india"}},
	{"Finnish", "fi", "fi_FI-harri-medium", []string{"finnish", "fi", "fi_fi", "finland", "suomi"}},
	{"Danish", "da", "da_DK-talesyntese-medium", []string{"danish", "da", "da_dk", "denmark", "dansk"}},
	// STT-only (no piper voice): transcription works, spoken replies fall back to text.
	{"Japanese", "ja", "", []string{"japanese", "ja", "japan"}},
	{"Korean", "ko", "", []string{"korean", "ko", "korea"}},
}

// lookupLanguage resolves a free-form query (country, language name, or code) to
// a Language. Matching is case-insensitive against the aliases.
func lookupLanguage(q string) (*Language, bool) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil, false
	}
	for i := range languages {
		for _, a := range languages[i].Aliases {
			if a == q {
				return &languages[i], true
			}
		}
	}
	return nil, false
}

// languageList returns a compact, comma-separated list of the primary names for
// help text.
func languageList() string {
	names := make([]string, 0, len(languages))
	for i := range languages {
		names = append(names, languages[i].Name)
	}
	return strings.Join(names, ", ")
}

// LanguagePolicy is the live, runtime-toggleable audio language shared by the
// STT (incoming) and TTS (outgoing) engines and set by the /speech command.
type LanguagePolicy struct{ cur atomic.Pointer[Language] }

func NewLanguagePolicy(def string) *LanguagePolicy {
	p := &LanguagePolicy{}
	l, ok := lookupLanguage(def)
	if !ok {
		l, _ = lookupLanguage("auto")
	}
	p.cur.Store(l)
	return p
}

func (p *LanguagePolicy) Current() *Language { return p.cur.Load() }
func (p *LanguagePolicy) Set(l *Language)    { p.cur.Store(l) }

// WhisperCode is the transcription language hint ("" = auto-detect).
func (p *LanguagePolicy) WhisperCode() string {
	if l := p.cur.Load(); l != nil {
		return l.Whisper
	}
	return ""
}

// PiperVoice is the synthesis voice for the current language ("" = no voice,
// so spoken replies aren't possible and fall back to text).
func (p *LanguagePolicy) PiperVoice() string {
	if l := p.cur.Load(); l != nil {
		return l.Piper
	}
	return ""
}

// Describe is a short human label for the current language, e.g. for /speech.
func (p *LanguagePolicy) Describe() string {
	l := p.cur.Load()
	if l == nil {
		return "auto-detect"
	}
	voice := l.Piper
	if voice == "" {
		voice = "(no voice — replies fall back to text)"
	}
	return fmt.Sprintf("%s — transcribe: %s, speak: %s", l.Name, whisperLabel(l.Whisper), voice)
}

func whisperLabel(code string) string {
	if code == "" {
		return "auto-detect"
	}
	return code
}
