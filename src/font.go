package main

import (
	"encoding/binary"
	"math"
	"os"
	"regexp"
	"strings"
)

// FntCharImage stores sprite and position
type FntCharImage struct {
	ofs, w uint16
	img    []Sprite
}

// TtfFont implements TTF font rendering on supported platforms
type TtfFont interface {
	SetColor(red float32, green float32, blue float32, alpha float32)
	Width(scale float32, fs string, argv ...interface{}) float32
	Printf(x, y float32, scale float32, align int32, blend bool, window [4]int32, fs string, argv ...interface{}) error
}

// Fnt is a interface for basic font information
type Fnt struct {
	images    map[int32]map[rune]*FntCharImage
	palettes  [][256]uint32
	coldepth  []byte
	ver, ver2 uint16
	Type      string
	BankType  string
	Size      [2]uint16
	Spacing   [2]int32
	colors    int32
	offset    [2]int32
	ttf       TtfFont
	paltex    *Texture
}

func newFnt() *Fnt {
	return &Fnt{
		images:   make(map[int32]map[rune]*FntCharImage),
		BankType: "palette",
	}
}

func loadFnt(filename string, height int32) (*Fnt, error) {
	if HasExtension(filename, ".fnt") {
		return loadFntV1(filename)
	}

	return loadFntV2(filename, height)
}

func loadFntV1(filename string) (*Fnt, error) {
	f := newFnt()
	f.images[0] = make(map[rune]*FntCharImage)

	fp, err := os.Open(filename)

	if err != nil {
		return nil, Error("File not found")
	}

	defer func() { chk(fp.Close()) }()

	//Read header
	buf := make([]byte, 12)
	n, err := fp.Read(buf)

	//Error reading file
	if err != nil {
		return nil, err
	}

	//Error is not a valid fnt file
	if string(buf[:n]) != "ElecbyteFnt\x00" {
		return nil, Error("Unrecognized FNT file: " + string(buf[:n]))
	}

	read := func(x interface{}) error {
		return binary.Read(fp, binary.LittleEndian, x)
	}

	if err := read(&f.ver); err != nil {
		return nil, err
	}

	if err := read(&f.ver2); err != nil {
		return nil, err
	}

	var pcxDataOffset, pcxDataLength, txtDataOffset, txtDataLength uint32
	if err := read(&pcxDataOffset); err != nil {
		return nil, err
	}

	if err := read(&pcxDataLength); err != nil {
		return nil, err
	}

	if err := read(&txtDataOffset); err != nil {
		return nil, err
	}

	if err := read(&txtDataLength); err != nil {
		return nil, err
	}

	spr := newSprite()
	if err := spr.readPcxHeader(fp, int64(pcxDataOffset)); err != nil {
		return nil, err
	}

	fp.Seek(int64(pcxDataOffset)+128, 0)
	px := make([]byte, pcxDataLength-128-768)
	if err := read(px); err != nil {
		return nil, err
	}

	spr.Pal = make([]uint32, 256)
	var rgb [3]byte
	for i := range spr.Pal {
		if err := read(rgb[:]); err != nil {
			return nil, err
		}
		spr.Pal[i] = uint32(rgb[2])<<16 | uint32(rgb[1])<<8 | uint32(rgb[0])
	}

	px = spr.RlePcxDecode(px)
	fp.Seek(int64(txtDataOffset), 0)
	buf = make([]byte, txtDataLength)
	if err := read(buf); err != nil {
		return nil, err
	}
	lines := SplitAndTrim(string(buf), "\n")
	i := 0
	mapflg, defflg := true, true
	for {
		var name string
		for ; i < len(lines); i++ {
			name, _ = SectionName(lines[i])
			if len(name) > 0 {
				i++
				break
			}
		}
		if len(name) == 0 {
			break
		}
		switch name {
		case "map":
			if mapflg {
				mapflg = false
				re := regexp.MustCompile(`(\S+)(?:\s+(\S+)(?:\s+(\S+))?)?`)
				ofs := uint16(0)
				w := int32(0)
				for ; i < len(lines); i++ {
					if len(lines[i]) > 0 && lines[i][0] == '[' {
						break
					}
					cap := re.FindStringSubmatch(strings.SplitN(lines[i], ";", 2)[0])
					if len(cap) > 0 {
						var c rune
						if len(cap[1]) >= 2 && cap[1][0] == '0' &&
							(cap[1][1] == 'X' || cap[1][1] == 'x') {
							hex := strings.ToLower(cap[1][2:])
							for _, r := range hex {
								if '0' <= r && r <= '9' {
									c = c<<4 | (r - '0')
								} else if 'a' <= r && r <= 'f' {
									c = c<<4 | (r - 'a' + 10)
								} else {
									break
								}
							}
						} else {
							c = rune(cap[1][0])
						}
						if len(cap[2]) > 0 {
							ofs = I32ToU16(Atoi(cap[2]))
						}
						fci := &FntCharImage{ofs: ofs}
						f.images[0][c] = fci
						if len(cap[3]) > 0 {
							w = Atoi(cap[3])
							if w < 0 {
								ofs += I32ToU16(int32(ofs) - w)
								w = 0 - w
							}
							fci.w = I32ToU16(w)
							ofs += fci.w - f.Size[0]
						} else {
							fci.w = f.Size[0]
						}
					}
					ofs += f.Size[0]
				}
			}
		case "def":
			if defflg {
				defflg = false
				is := NewIniSection()
				is.Parse(lines, &i)
				loadDefInfo(f, filename, is, 0)
			}
		}
	}
	c := Min(255, int32(math.Ceil(float64(f.colors)/16))*16)
	f.palettes = make([][256]uint32, 255/c)
	for i := int32(0); int(i) < len(f.palettes); i++ {
		copy(f.palettes[i][:256-c], spr.Pal[:256-c])
		copy(f.palettes[i][256-c:], spr.Pal[256-c*(i+1):256-c*i])
	}
	copyCharRect := func(dst []byte, dw int, src []byte, x, w, h int) {
		dw2 := dw
		if x+dw > w {
			dw2 = w - x
		}
		if dw2 > 0 {
			for i := 0; i < h; i++ {
				copy(dst[dw*i:dw*i+dw2], src[w*i+x:w*i+x+dw2])
			}
		}
	}
	for _, fci := range f.images[0] {
		fci.img = make([]Sprite, len(f.palettes))
		for i, p := range f.palettes {
			if i == 0 {
				fci.img[0].shareCopy(spr)
				fci.img[0].Size[0] = fci.w
				px2 := make([]byte, int(fci.w)*int(fci.img[0].Size[1]))
				copyCharRect(px2, int(fci.w), px, int(fci.ofs),
					int(spr.Size[0]), int(spr.Size[1]))
				fci.img[0].SetPxl(px2)
			} else {
				i, fci := i, fci
				sys.mainThreadTask <- func() {
					fci.img[i].shareCopy(&fci.img[0])
					fci.img[i].Size[0] = fci.w
				}
			}
			fci.img[i].Offset[0], fci.img[i].Offset[1], fci.img[i].Pal = 0, 0, p[:]
		}
	}
	return f, nil
}

func loadFntV2(filename string, height int32) (*Fnt, error) {
	f := newFnt()

	content, err := LoadText(SearchFile(filename, []string{""}))

	if err != nil {
		return nil, Error("File not found")
	}

	lines := SplitAndTrim(string(content), "\n")
	i := 0
	var name string

	for ; i < len(lines); i++ {
		name, _ = SectionName(lines[i])
		if len(name) > 0 {
			is := NewIniSection()
			i++
			is.Parse(lines, &i)
			i--
			switch name {
			case "def":
				loadDefInfo(f, filename, is, height)
			}
		}
	}
	return f, nil
}

func loadDefInfo(f *Fnt, filename string, is IniSection, height int32) {
	f.Type = strings.ToLower(is["type"])
	if _, ok := is["banktype"]; ok {
		f.BankType = strings.ToLower(is["banktype"])
	}
	ary := SplitAndTrim(is["size"], ",")
	if len(ary[0]) > 0 {
		f.Size[0] = I32ToU16(Atoi(ary[0]))
	}
	if len(ary) > 1 && len(ary[1]) > 0 {
		f.Size[1] = I32ToU16(Atoi(ary[1]))
	}
	ary = SplitAndTrim(is["spacing"], ",")
	if len(ary[0]) > 0 {
		f.Spacing[0] = Atoi(ary[0])
	}
	if len(ary) > 1 && len(ary[1]) > 0 {
		f.Spacing[1] = Atoi(ary[1])
	}
	f.colors = Clamp(Atoi(is["colors"]), 1, 255)
	ary = SplitAndTrim(is["offset"], ",")
	if len(ary[0]) > 0 {
		f.offset[0] = Atoi(ary[0])
	}
	if len(ary) > 1 && len(ary[1]) > 0 {
		f.offset[1] = Atoi(ary[1])
	}

	if len(is["file"]) > 0 {
		if f.Type == "truetype" {
			LoadFntTtf(f, filename, is["file"], height)
		} else {
			LoadFntSff(f, filename, is["file"])
		}
	}
}

func LoadFntSff(f *Fnt, fontfile string, filename string) {
	fileDir := SearchFile(filename, []string{fontfile, "font/", sys.motifDir, "", "data/"})
	sff, err := loadSff(fileDir, false)

	if err != nil {
		panic(err)
	}

	//Load sprites
	var pal_default []uint32
	for k, sprite := range sff.sprites {
		s := sff.getOwnPalSprite(sprite.Group, sprite.Number, &sff.palList)
		if sprite.Group == 0 || f.BankType == "sprite" {
			if f.images[int32(sprite.Group)] == nil {
				f.images[int32(sprite.Group)] = make(map[rune]*FntCharImage)
			}
			if pal_default == nil && sff.header.Ver0 == 1 {
				pal_default = s.Pal
			}
			offsetX := uint16(s.Offset[0])
			sizeX := uint16(s.Size[0])

			fci := &FntCharImage{
				ofs: offsetX,
				w:   sizeX,
			}
			fci.img = make([]Sprite, 1)
			fci.img[0] = *s
			f.images[int32(sprite.Group)][rune(k[1])] = fci
		}
	}

	//Load palettes
	f.palettes = make([][256]uint32, sff.header.NumberOfPalettes)
	f.coldepth = make([]byte, sff.header.NumberOfPalettes)
	var idef int
	for i := 0; i < int(sff.header.NumberOfPalettes); i++ {
		var pal []uint32
		si, ok := sff.palList.PalTable[[...]int16{0, int16(i)}]
		if ok && si >= 0 {
			pal = sff.palList.Get(si)
			if i == 0 {
				idef = si
			}
			switch sff.palList.numcols[[...]int16{0, int16(i)}] {
			case 256:
				f.coldepth[i] = 8
			case 32:
				f.coldepth[i] = 5
			}
		} else {
			pal = sff.palList.Get(idef)
		}
		copy(f.palettes[i][:], pal)
	}
	if len(f.palettes) == 0 && pal_default != nil {
		f.palettes = make([][256]uint32, 1)
		copy(f.palettes[0][:], pal_default)
	}
}

// CharWidth returns the width that has a specified character
func (f *Fnt) CharWidth(c rune, bt int32) int32 {
	if c == ' ' {
		return int32(f.Size[0])
	}
	fci := f.images[bt][c]
	if fci == nil {
		return 0
	}
	return int32(fci.w)
}

// TextWidth returns the width that has a specified text.
// This depends on each char's width and font spacing
func (f *Fnt) TextWidth(txt string, bank int32) (w int32) {
	if f.BankType != "sprite" {
		bank = 0
	}
	for i, c := range txt {
		if f.Type == "truetype" {
			w += int32(f.ttf.Width(1, string(c)))
		} else {
			cw := f.CharWidth(c, bank)
			// in mugen negative spacing matching char width seems to skip calc,
			// even for 1 symbol string (which normally shouldn't use spacing)
			if cw+f.Spacing[0] > 0 {
				w += cw
				if i < len(txt)-1 {
					w += f.Spacing[0]
				}
			}
		}
	}
	return
}

func (f *Fnt) getCharSpr(c rune, bank, bt int32) *Sprite {
	fci := f.images[bt][c]
	if fci == nil {
		return nil
	}

	if bank < int32(len(fci.img)) {
		return &fci.img[bank]
	}

	return &fci.img[0]
}

func (f *Fnt) drawChar(
	x, y,
	xscl, yscl float32,
	bank, bt int32,
	c rune, pal []uint32,
	window *[4]int32,
	palfx *PalFX,
) float32 {
	if c == ' ' {
		return float32(f.Size[0]) * xscl
	}

	spr := f.getCharSpr(c, bank, bt)
	if spr == nil || spr.Tex == nil {
		return 0
	}

	// in case of mismatched color depth between bank palette and
	// sprite own palette, mugen 1.1 uses the latter, ignoring bank
	if len(f.palettes) != 0 && len(f.coldepth) > int(bank) &&
		f.images[bt][c].img[0].coldepth != 32 &&
		f.coldepth[bank] != f.images[bt][c].img[0].coldepth {
		pal = palfx.getFxPal(f.images[bt][c].img[0].Pal[:], false)
	}

	x -= xscl * float32(spr.Offset[0])
	y -= yscl * float32(spr.Offset[1])
	if spr.coldepth <= 8 && f.paltex == nil {
		f.paltex = spr.CachePalette(pal)
	}
	rp := RenderParams{
		spr.Tex, f.paltex, spr.Size,
		-x * sys.widthScale, -y * sys.heightScale, notiling,
		xscl * sys.widthScale, xscl * sys.widthScale,
		yscl * sys.heightScale, 1, 0,
		Rotation{},
		0, sys.brightness*255>>8 | 1<<9, 0,
		nil, window, 0, 0,
		0, 0, -xscl * float32(spr.Offset[0]), -yscl * float32(spr.Offset[1]),
	}
	RenderSprite(rp)
	return float32(spr.Size[0]) * xscl
}

func (f *Fnt) Print(txt string, x, y, xscl, yscl float32, bank, align int32,
	window *[4]int32, palfx *PalFX, frgba [4]float32) {
	if !sys.frameSkip {
		if f.Type == "truetype" {
			f.DrawTtf(txt, x, y, xscl, yscl, align, true, window, frgba)
		} else {
			f.DrawText(txt, x, y, xscl, yscl, bank, align, window, palfx)
		}
	}
}

// DrawText prints on screen a specified text with the current font sprites
func (f *Fnt) DrawText(txt string, x, y, xscl, yscl float32, bank, align int32,
	window *[4]int32, palfx *PalFX) {

	if len(txt) == 0 {
		return
	}

	var bt int32
	if f.BankType == "sprite" {
		bt = bank
		bank = 0
	} else if bank < 0 || len(f.palettes) <= int(bank) {
		bank = 0
	}

	//not existing characters treated as space
	for i, c := range txt {
		if c != ' ' && f.images[bt][c] == nil {
			//txt = strings.Replace(txt, string(c), " ", -1)
			txt = txt[:i] + string(' ') + txt[i+1:]
		}
	}

	x += float32(f.offset[0])*xscl + float32(sys.gameWidth-320)/2
	y += float32(f.offset[1]-int32(f.Size[1])+1)*yscl + float32(sys.gameHeight-240)

	if align == 0 {
		x -= float32(f.TextWidth(txt, bank)) * xscl * 0.5
	} else if align < 0 {
		x -= float32(f.TextWidth(txt, bank)) * xscl
	}

	var pal []uint32
	if len(f.palettes) != 0 {
		pal = palfx.getFxPal(f.palettes[bank][:], false)
	}

	f.paltex = nil
	for _, c := range txt {
		x += f.drawChar(x, y, xscl, yscl, bank, bt, c, pal, window, palfx) + xscl*float32(f.Spacing[0])
	}
}

func (f *Fnt) DrawTtf(txt string, x, y, xscl, yscl float32, align int32,
	blend bool, window *[4]int32, frgba [4]float32) {

	if len(txt) == 0 {
		return
	}

	x += float32(f.offset[0])*xscl + float32(sys.gameWidth-320)/2
	//y += float32(f.offset[1]-int32(f.Size[1])+1)*yscl + float32(sys.gameHeight-240)

	win := [4]int32{(*window)[0], sys.scrrect[3] - ((*window)[1] + (*window)[3]),
		(*window)[2], (*window)[3]}

	f.ttf.SetColor(frgba[0], frgba[1], frgba[2], frgba[3])
	f.ttf.Printf(x, y, (xscl+yscl)/2, align, blend, win, "%s", txt) //x, y, scale, align, blend, window, string, printf args
}

type TextSprite struct {
	text             string
	fnt              *Fnt
	bank, align      int32
	x, y, xscl, yscl float32
	window           [4]int32
	palfx            *PalFX
	frgba            [4]float32 //ttf fonts
	removetime       int32      //text sctrl
	layerno          int16      //text sctrl
	localScale       float32    //text sctrl
	offsetX          int32      //text sctrl
}

func NewTextSprite() *TextSprite {
	ts := &TextSprite{
		align:      1,
		x:          sys.luaSpriteOffsetX,
		xscl:       1,
		yscl:       1,
		window:     sys.scrrect,
		palfx:      newPalFX(),
		frgba:      [...]float32{1.0, 1.0, 1.0, 1.0},
		removetime: 1,
		layerno:    1,
		localScale: 1,
		offsetX:    0,
	}
	ts.palfx.setColor(255, 255, 255)
	return ts
}

func (ts *TextSprite) SetLocalcoord(lx, ly float32) {
	v := lx
	if lx*3 > ly*4 {
		v = ly * 4 / 3
	}
	ts.localScale = float32(v / 320)
	ts.offsetX = -int32(math.Floor(float64(lx)/(float64(v)/320)-320) / 2)
}

func (ts *TextSprite) SetWindow(x, y, w, h float32) {
	ts.window[0] = int32((x + float32(sys.gameWidth-320)/2) * sys.widthScale)
	ts.window[1] = int32((y + float32(sys.gameHeight-240)) * sys.heightScale)
	ts.window[2] = int32(w*sys.widthScale + 0.5)
	ts.window[3] = int32(h*sys.heightScale + 0.5)
}

func (ts *TextSprite) SetColor(r, g, b int32) {
	ts.palfx.setColor(r, g, b)
	ts.frgba = [...]float32{float32(r) / 255, float32(g) / 255,
		float32(b) / 255, 1.0}
}

func (ts *TextSprite) Draw() {
	if !sys.frameSkip && ts.fnt != nil {
		if ts.fnt.Type == "truetype" {
			ts.fnt.DrawTtf(ts.text, ts.x, ts.y, ts.xscl, ts.yscl, ts.align, true, &ts.window, ts.frgba)
		} else {
			ts.fnt.DrawText(ts.text, ts.x, ts.y, ts.xscl, ts.yscl, ts.bank, ts.align, &ts.window, ts.palfx)
		}
	}
}
