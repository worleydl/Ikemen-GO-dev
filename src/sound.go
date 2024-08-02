package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"

	"github.com/ikemen-engine/beep"
	"github.com/ikemen-engine/beep/effects"

	"github.com/ikemen-engine/beep/midi"
	"github.com/ikemen-engine/beep/mp3"
	"github.com/ikemen-engine/beep/speaker"
	"github.com/ikemen-engine/beep/vorbis"
	"github.com/ikemen-engine/beep/wav"
)

const (
	audioOutLen          = 2048
	audioFrequency       = 48000
	audioPrecision       = 4
	audioResampleQuality = 1
	audioSoundFont       = "sound/soundfont.sf2" // default path for MIDI soundfont
)

// ------------------------------------------------------------------
// Normalizer

type Normalizer struct {
	streamer beep.Streamer
	mul      float64
	l, r     *NormalizerLR
}

func NewNormalizer(st beep.Streamer) *Normalizer {
	return &Normalizer{streamer: st, mul: 4,
		l: &NormalizerLR{1, 0, 1, 1 / 32.0, 0, 0},
		r: &NormalizerLR{1, 0, 1, 1 / 32.0, 0, 0}}
}

func (n *Normalizer) Stream(samples [][2]float64) (s int, ok bool) {
	s, ok = n.streamer.Stream(samples)
	for i := range samples[:s] {
		lmul := n.l.process(n.mul, &samples[i][0])
		rmul := n.r.process(n.mul, &samples[i][1])
		if sys.audioDucking {
			n.mul = math.Min(16.0, math.Min(lmul, rmul))
		} else {
			n.mul = 0.5 * (float64(sys.wavVolume) * float64(sys.masterVolume) * 0.0001)
		}
	}
	return s, ok
}

func (n *Normalizer) Err() error {
	return n.streamer.Err()
}

type NormalizerLR struct {
	edge, edgeDelta, gain, average, bias, bias2 float64
}

func (n *NormalizerLR) process(mul float64, sam *float64) float64 {
	n.bias += (*sam - n.bias) / (audioFrequency/110.0 + 1)
	n.bias2 += (*sam - n.bias2) / (audioFrequency/112640.0 + 1)
	s := (n.bias2 - n.bias) * mul
	if math.Abs(s) > 1 {
		mul *= math.Pow(math.Abs(s), -n.edge)
		n.edgeDelta += 32 * (1 - n.edge) / float64(audioFrequency+32)
		s = math.Copysign(1.0, s)
	} else {
		tmp := (1 - math.Pow(1-math.Abs(s), 64)) * math.Pow(0.5-math.Abs(s), 3)
		mul += mul * (n.edge*(1/32.0-n.average)/n.gain + tmp*n.gain*(1-n.edge)/32) /
			(audioFrequency*2/8.0 + 1)
		n.edgeDelta -= (0.5 - n.average) * n.edge / (audioFrequency * 2)
	}
	n.gain += (1.0 - n.gain*(math.Abs(s)+1/32.0)) / (audioFrequency * 2)
	n.average += (math.Abs(s) - n.average) / (audioFrequency * 2)
	n.edge = float64(ClampF(float32(n.edge+n.edgeDelta), 0, 1))
	*sam = s
	return mul
}

// ------------------------------------------------------------------
// Loop Streamer

// Based on Loop() from Beep package. It adds support for loop points.
type StreamLooper struct {
	s         beep.StreamSeeker
	loopcount int
	loopstart int
	loopend   int
}

func newStreamLooper(s beep.StreamSeeker, loopcount, loopstart, loopend int) beep.Streamer {
	if loopstart < 0 || loopstart >= s.Len() {
		loopstart = 0
	}
	if loopend <= loopstart {
		loopend = s.Len()
	}
	return &StreamLooper{
		s:         s,
		loopcount: loopcount,
		loopstart: loopstart,
		loopend:   loopend,
	}
}

func (b *StreamLooper) Stream(samples [][2]float64) (n int, ok bool) {
	if b.loopcount == 0 || b.s.Err() != nil {
		return 0, false
	}
	for len(samples) > 0 {
		sn, sok := b.s.Stream(samples)
		if !sok || (b.s.Position() >= b.loopend && b.loopend < b.s.Len()) {
			if b.loopcount > 0 {
				b.loopcount--
			}
			if b.loopcount == 0 {
				break
			}
			err := b.s.Seek(b.loopstart)
			if err != nil {
				return n, true
			}
			continue
		}
		samples = samples[sn:]
		n += sn
	}
	return n, true
}

func (b *StreamLooper) Err() error {
	return b.s.Err()
}

func (b *StreamLooper) Len() int {
	return b.s.Len()
}

func (b *StreamLooper) Position() int {
	return b.s.Position()
}

func (b *StreamLooper) Seek(p int) error {
	return b.s.Seek(p)
}

// ------------------------------------------------------------------
// Bgm

type Bgm struct {
	filename   string
	bgmVolume  int
	volRestore int
	loop       int
	streamer   beep.StreamSeekCloser
	ctrl       *beep.Ctrl
	volctrl    *effects.Volume
	format     string
	freqmul    float32
	sampleRate beep.SampleRate
	startPos   int
}

func newBgm() *Bgm {
	return &Bgm{}
}

func (bgm *Bgm) Open(filename string, loop, bgmVolume, bgmLoopStart, bgmLoopEnd, startPosition int, freqmul float32) {
	bgm.filename = filename
	bgm.loop = loop
	bgm.bgmVolume = bgmVolume
	bgm.freqmul = freqmul
	// Starve the current music streamer
	if bgm.ctrl != nil {
		speaker.Lock()
		bgm.ctrl.Streamer = nil
		speaker.Unlock()
	}
	// Special value "" is used to stop music
	if filename == "" {
		return
	}

	f, err := os.Open(bgm.filename)
	if err != nil {
		// sys.bgm = *newBgm() // removing this gets pause step playsnd to work correctly 100% of the time
		sys.errLog.Printf("Failed to open bgm: %v", err)
		return
	}
	var format beep.Format
	if HasExtension(bgm.filename, ".ogg") {
		bgm.streamer, format, err = vorbis.Decode(f)
		bgm.format = "ogg"
	} else if HasExtension(bgm.filename, ".mp3") {
		bgm.streamer, format, err = mp3.Decode(f)
		bgm.format = "mp3"
	} else if HasExtension(bgm.filename, ".wav") {
		bgm.streamer, format, err = wav.Decode(f)
		bgm.format = "wav"
		// TODO: Reactivate FLAC support. Check that seeking/looping works correctly.
		//} else if HasExtension(bgm.filename, ".flac") {
		//	bgm.streamer, format, err = flac.Decode(f)
		//	bgm.format = "flac"
	} else if HasExtension(bgm.filename, ".mid") || HasExtension(bgm.filename, ".midi") {
		if soundfont, sferr := loadSoundFont(audioSoundFont); sferr != nil {
			err = sferr
		} else {
			bgm.streamer, format, err = midi.Decode(f, soundfont)
			bgm.format = "midi"
		}
	} else {
		err = Error(fmt.Sprintf("unsupported file extension: %v", bgm.filename))
	}
	if err != nil {
		f.Close()
		sys.errLog.Printf("Failed to load bgm: %v", err)
		return
	}

	loopCount := int(1)
	if loop > 0 {
		loopCount = -1
	}
	bgm.startPos = startPosition
	streamer := newStreamLooper(bgm.streamer, loopCount, bgmLoopStart, bgmLoopEnd)
	bgm.volctrl = &effects.Volume{Streamer: streamer, Base: 2, Volume: 0, Silent: true}
	bgm.sampleRate = format.SampleRate
	dstFreq := beep.SampleRate(audioFrequency / bgm.freqmul)
	resampler := beep.Resample(audioResampleQuality, bgm.sampleRate, dstFreq, bgm.volctrl)
	bgm.ctrl = &beep.Ctrl{Streamer: resampler}
	bgm.UpdateVolume()
	bgm.streamer.Seek(startPosition)
	speaker.Play(bgm.ctrl)
}

func loadSoundFont(filename string) (*midi.SoundFont, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	soundfont, err := midi.NewSoundFont(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return soundfont, nil
}

func (bgm *Bgm) SetPaused(pause bool) {
	if bgm.ctrl == nil || bgm.ctrl.Paused == pause {
		return
	}
	speaker.Lock()
	bgm.ctrl.Paused = pause
	speaker.Unlock()
}

func (bgm *Bgm) UpdateVolume() {
	if bgm.volctrl == nil {
		return
	}
	// TODO: Throw a debug warning if this triggers
	if bgm.bgmVolume > sys.maxBgmVolume {
		sys.errLog.Printf("WARNING: BGM volume set beyond expected range (value: %v). Clamped to MaxBgmVolume", bgm.bgmVolume)
		bgm.bgmVolume = sys.maxBgmVolume
	}
	volume := -5 + float64(sys.bgmVolume)*0.06*(float64(sys.masterVolume)/100)*(float64(bgm.bgmVolume)/100)
	silent := volume <= -5
	speaker.Lock()
	bgm.volctrl.Volume = volume
	bgm.volctrl.Silent = silent
	speaker.Unlock()
}

func (bgm *Bgm) SetFreqMul(freqmul float32) {
	if bgm.freqmul != freqmul {
		if bgm.ctrl != nil {
			srcRate := bgm.sampleRate
			dstRate := beep.SampleRate(audioFrequency / freqmul)
			if resampler, ok := bgm.ctrl.Streamer.(*beep.Resampler); ok {
				speaker.Lock()
				resampler.SetRatio(float64(srcRate) / float64(dstRate))
				bgm.freqmul = freqmul
				speaker.Unlock()
			}
		}
	}
}

func (bgm *Bgm) SetLoopPoints(bgmLoopStart int, bgmLoopEnd int) {
	// Set both at once, why not
	if sl, ok := bgm.volctrl.Streamer.(*StreamLooper); ok {
		if sl.loopstart != bgmLoopStart && sl.loopend != bgmLoopEnd {
			speaker.Lock()
			sl.loopstart = bgmLoopStart
			sl.loopend = bgmLoopEnd
			speaker.Unlock()
			// Set one at a time
		} else {
			if sl.loopstart != bgmLoopStart {
				speaker.Lock()
				sl.loopstart = bgmLoopStart
				speaker.Unlock()
			} else if sl.loopend != bgmLoopEnd {
				speaker.Lock()
				sl.loopend = bgmLoopEnd
				speaker.Unlock()
			}
		}
	}
}

func (bgm *Bgm) Seek(positionSample int) {
	speaker.Lock()
	// Reset to 0 if out of range
	if positionSample < 0 || positionSample > bgm.streamer.Len() {
		positionSample = 0
	}
	bgm.streamer.Seek(positionSample)
	speaker.Unlock()
}

// ------------------------------------------------------------------
// Sound

type Sound struct {
	wavData []byte
	format  beep.Format
	length  int
}

func readSound(f *os.File, size uint32) (*Sound, error) {
	if size < 128 {
		return nil, fmt.Errorf("wav size is too small")
	}
	wavData := make([]byte, size)
	if _, err := f.Read(wavData); err != nil {
		return nil, err
	}
	// Decode the sound at least once, so that we know the format is OK
	s, fmt, err := wav.Decode(bytes.NewReader(wavData))
	if err != nil {
		return nil, err
	}
	// Check if the file can be fully played
	var samples [512][2]float64
	for {
		sn, _ := s.Stream(samples[:])
		if sn == 0 {
			// If sound wasn't able to be fully played, we disable it to avoid engine freezing
			if s.Position() < s.Len() {
				return nil, nil
			}
			break
		}
	}
	return &Sound{wavData, fmt, s.Len()}, nil
}

func (s *Sound) GetStreamer() beep.StreamSeeker {
	streamer, _, _ := wav.Decode(bytes.NewReader(s.wavData))
	return streamer
}

// ------------------------------------------------------------------
// Snd

type Snd struct {
	table     map[[2]int32]*Sound
	ver, ver2 uint16
}

func newSnd() *Snd {
	return &Snd{table: make(map[[2]int32]*Sound)}
}

func LoadSnd(filename string) (*Snd, error) {
	return LoadSndFiltered(filename, func(gn [2]int32) bool { return gn[0] >= 0 && gn[1] >= 0 }, 0)
}

// Parse a .snd file and return an Snd structure with its contents
// The "keepItem" function allows to filter out unwanted waves.
// If max > 0, the function returns immediately when a matching entry is found. It also gives up after "max" non-matching entries.
func LoadSndFiltered(filename string, keepItem func([2]int32) bool, max uint32) (*Snd, error) {
	s := newSnd()
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { chk(f.Close()) }()
	buf := make([]byte, 12)
	var n int
	if n, err = f.Read(buf); err != nil {
		return nil, err
	}
	if string(buf[:n]) != "ElecbyteSnd\x00" {
		return nil, Error("Unrecognized SND file, invalid header")
	}
	read := func(x interface{}) error {
		return binary.Read(f, binary.LittleEndian, x)
	}
	if err := read(&s.ver); err != nil {
		return nil, err
	}
	if err := read(&s.ver2); err != nil {
		return nil, err
	}
	var numberOfSounds uint32
	if err := read(&numberOfSounds); err != nil {
		return nil, err
	}
	var subHeaderOffset uint32
	if err := read(&subHeaderOffset); err != nil {
		return nil, err
	}
	loops := numberOfSounds
	if max > 0 && max < numberOfSounds {
		loops = max
	}
	for i := uint32(0); i < loops; i++ {
		f.Seek(int64(subHeaderOffset), 0)
		var nextSubHeaderOffset uint32
		if err := read(&nextSubHeaderOffset); err != nil {
			return nil, err
		}
		var subFileLength uint32
		if err := read(&subFileLength); err != nil {
			return nil, err
		}
		var num [2]int32
		if err := read(&num); err != nil {
			return nil, err
		}
		if keepItem(num) {
			_, ok := s.table[num]
			if !ok {
				tmp, err := readSound(f, subFileLength)
				if err != nil {
					sys.errLog.Printf("%v sound %v,%v can't be read: %v\n", filename, num[0], num[1], err)
					if max > 0 {
						return nil, err
					}
				} else {
					// Sound is corrupted and can't be played, so we export a warning message to the console
					if tmp == nil {
						sys.appendToConsole(fmt.Sprintf("WARNING: %v sound %v,%v is corrupted and can't be played, so it was disabled", filename, num[0], num[1]))
					}
					s.table[num] = tmp
					if max > 0 {
						break
					}
				}
			}
		}
		subHeaderOffset = nextSubHeaderOffset
	}
	return s, nil
}
func (s *Snd) Get(gn [2]int32) *Sound {
	return s.table[gn]
}
func (s *Snd) play(gn [2]int32, volumescale int32, pan float32, loopstart, loopend, startposition int) bool {
	sound := s.Get(gn)
	return sys.soundChannels.Play(sound, volumescale, pan, loopstart, loopend, startposition)
}
func (s *Snd) stop(gn [2]int32) {
	sound := s.Get(gn)
	sys.soundChannels.Stop(sound)
}

func loadFromSnd(filename string, g, s int32, max uint32) (*Sound, error) {
	// Load the snd file
	snd, err := LoadSndFiltered(filename, func(gn [2]int32) bool { return gn[0] == g && gn[1] == s }, max)
	if err != nil {
		return nil, err
	}
	tmp, ok := snd.table[[2]int32{g, s}]
	if !ok {
		return nil, nil
	}
	return tmp, nil
}

// ------------------------------------------------------------------
// SoundEffect (handles volume and panning)

type SoundEffect struct {
	streamer beep.Streamer
	volume   float32
	ls, p    float32
	x        *float32
	priority int32
	channel  int32
	loop     int32
	freqmul  float32
}

func (s *SoundEffect) Stream(samples [][2]float64) (n int, ok bool) {
	// TODO: Test mugen panning in relation to PanningWidth and zoom settings
	lv, rv := s.volume, s.volume
	if sys.stereoEffects && (s.x != nil || s.p != 0) {
		var r float32
		if s.x != nil { // pan
			r = ((sys.xmax - s.ls**s.x) - s.p) / (sys.xmax - sys.xmin)
		} else { // abspan
			r = ((sys.xmax-sys.xmin)/2 - s.p) / (sys.xmax - sys.xmin)
		}
		sc := sys.panningRange / 100
		of := (100 - sys.panningRange) / 200
		lv = ClampF(s.volume*2*(r*sc+of), 0, 512)
		rv = ClampF(s.volume*2*((1-r)*sc+of), 0, 512)
	}

	n, ok = s.streamer.Stream(samples)
	for i := range samples[:n] {
		samples[i][0] *= float64(lv / 256)
		samples[i][1] *= float64(rv / 256)
	}
	return n, ok
}

func (s *SoundEffect) Err() error {
	return s.streamer.Err()
}

// ------------------------------------------------------------------
// SoundChannel

type SoundChannel struct {
	streamer          beep.StreamSeeker
	sfx               *SoundEffect
	ctrl              *beep.Ctrl
	sound             *Sound
	stopOnGetHit      bool
	stopOnChangeState bool
}

func (s *SoundChannel) Play(sound *Sound, loop int32, freqmul float32, loopStart, loopEnd, startPosition int) {
	if sound == nil {
		return
	}
	s.sound = sound
	s.streamer = s.sound.GetStreamer()
	loopCount := int(1)
	if loop < 0 {
		loopCount = -1
	} else {
		loopCount = int(Max(loop, 1))
	}
	looper := newStreamLooper(s.streamer, loopCount, loopStart, loopEnd)
	s.sfx = &SoundEffect{streamer: looper, volume: 256, priority: 0, channel: -1, loop: int32(loopCount), freqmul: freqmul}
	srcRate := s.sound.format.SampleRate
	dstRate := beep.SampleRate(audioFrequency / s.sfx.freqmul)
	resampler := beep.Resample(audioResampleQuality, srcRate, dstRate, s.sfx)
	s.ctrl = &beep.Ctrl{Streamer: resampler}
	s.streamer.Seek(startPosition)
	sys.soundMixer.Add(s.ctrl)
}
func (s *SoundChannel) IsPlaying() bool {
	return s.sound != nil
}
func (s *SoundChannel) SetPaused(pause bool) {
	if s.ctrl == nil || s.ctrl.Paused == pause {
		return
	}
	speaker.Lock()
	s.ctrl.Paused = pause
	speaker.Unlock()
}
func (s *SoundChannel) Stop() {
	if s.ctrl != nil {
		speaker.Lock()
		s.ctrl.Streamer = nil
		speaker.Unlock()
	}
	s.sound = nil
}
func (s *SoundChannel) SetVolume(vol float32) {
	if s.ctrl != nil {
		s.sfx.volume = ClampF(vol, 0, 512)
	}
}
func (s *SoundChannel) SetPan(p, ls float32, x *float32) {
	if s.ctrl != nil {
		s.sfx.ls = ls
		s.sfx.x = x
		s.sfx.p = p * ls
	}
}
func (s *SoundChannel) SetPriority(priority int32) {
	if s.ctrl != nil {
		s.sfx.priority = priority
	}
}
func (s *SoundChannel) SetChannel(channel int32) {
	if s.ctrl != nil {
		s.sfx.channel = channel
	}
}
func (s *SoundChannel) SetFreqMul(freqmul float32) {
	if s.ctrl != nil {
		if s.sound != nil {
			srcRate := s.sound.format.SampleRate
			dstRate := beep.SampleRate(audioFrequency / freqmul)
			if resampler, ok := s.ctrl.Streamer.(*beep.Resampler); ok {
				speaker.Lock()
				resampler.SetRatio(float64(srcRate) / float64(dstRate))
				s.sfx.freqmul = freqmul
				speaker.Unlock()
			}
		}
	}
}
func (s *SoundChannel) SetLoopPoints(loopstart, loopend int) {
	// Set both at once, why not
	if sl, ok := s.sfx.streamer.(*StreamLooper); ok {
		if sl.loopstart != loopstart && sl.loopend != loopend {
			speaker.Lock()
			sl.loopstart = loopstart
			sl.loopend = loopend
			speaker.Unlock()
			// Set one at a time
		} else {
			if sl.loopstart != loopstart {
				speaker.Lock()
				sl.loopstart = loopstart
				speaker.Unlock()
			} else if sl.loopend != loopend {
				speaker.Lock()
				sl.loopend = loopend
				speaker.Unlock()
			}
		}
	}
}

// ------------------------------------------------------------------
// SoundChannels (collection of prioritised sound channels)

type SoundChannels struct {
	channels  []SoundChannel
	volResume []float32
}

func newSoundChannels(size int32) *SoundChannels {
	s := &SoundChannels{}
	s.SetSize(size)
	return s
}
func (s *SoundChannels) SetSize(size int32) {
	if size > s.count() {
		c := make([]SoundChannel, size-s.count())
		v := make([]float32, size-s.count())
		s.channels = append(s.channels, c...)
		s.volResume = append(s.volResume, v...)
	} else if size < s.count() {
		for i := s.count() - 1; i >= size; i-- {
			s.channels[i].Stop()
		}
		s.channels = s.channels[:size]
		s.volResume = s.volResume[:size]
	}
}
func (s *SoundChannels) count() int32 {
	return int32(len(s.channels))
}
func (s *SoundChannels) New(ch int32, lowpriority bool, priority int32) *SoundChannel {
	if ch >= 0 && ch < sys.wavChannels {
		for i := s.count() - 1; i >= 0; i-- {
			if s.channels[i].IsPlaying() && s.channels[i].sfx.channel == ch {
				if (lowpriority && priority <= s.channels[i].sfx.priority) || priority < s.channels[i].sfx.priority {
					return nil
				}
				s.channels[i].Stop()
				return &s.channels[i]
			}
		}
	}
	if s.count() < sys.wavChannels {
		s.SetSize(sys.wavChannels)
	}
	for i := sys.wavChannels - 1; i >= 0; i-- {
		if !s.channels[i].IsPlaying() {
			return &s.channels[i]
		}
	}
	return nil
}
func (s *SoundChannels) reserveChannel() *SoundChannel {
	for i := range s.channels {
		if !s.channels[i].IsPlaying() {
			return &s.channels[i]
		}
	}
	return nil
}
func (s *SoundChannels) Get(ch int32) *SoundChannel {
	if ch >= 0 && ch < s.count() {
		for i := range s.channels {
			if s.channels[i].IsPlaying() && s.channels[i].sfx != nil && s.channels[i].sfx.channel == ch {
				return &s.channels[i]
			}
		}
		//return &s.channels[ch]
	}
	return nil
}
func (s *SoundChannels) Play(sound *Sound, volumescale int32, pan float32, loopStart, loopEnd, startPosition int) bool {
	if sound == nil {
		return false
	}
	c := s.reserveChannel()
	if c == nil {
		return false
	}
	c.Play(sound, 0, 1.0, loopStart, loopEnd, startPosition)
	c.SetVolume(float32(volumescale * 64 / 25))
	c.SetPan(pan, 0, nil)
	return true
}
func (s *SoundChannels) IsPlaying(sound *Sound) bool {
	for _, v := range s.channels {
		if v.sound != nil && v.sound == sound {
			return true
		}
	}
	return false
}
func (s *SoundChannels) Stop(sound *Sound) {
	for k, v := range s.channels {
		if v.sound != nil && v.sound == sound {
			s.channels[k].Stop()
		}
	}
}
func (s *SoundChannels) StopAll() {
	for k, v := range s.channels {
		if v.sound != nil {
			s.channels[k].Stop()
		}
	}
}
func (s *SoundChannels) Tick() {
	for i := range s.channels {
		if s.channels[i].IsPlaying() {
			if s.channels[i].streamer.Position() >= s.channels[i].sound.length && s.channels[i].sfx.loop != -1 {
				s.channels[i].sound = nil
			}
		}
	}
}
