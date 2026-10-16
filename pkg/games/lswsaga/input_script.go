package lswsaga

import "catalina32/pkg/d3d9"

// inputScript drives unattended C32_AUTO_KEYS runs through this game's menus and levels:
//
//	boot     Title -> Main Menu -> New Game -> Save Slot 1 -> Mos Eisley Cantina
//	episode1 boot, then walk into Episode I Chapter 1 (Negotiations)
//	outside  boot, then walk out of the Cantina into the Mos Eisley courtyard
//	loop     periodic Enter/F2 taps
//
// Keys use the default layout: P1 = W/A/S/D, U = jump/confirm, Enter = start.
func inputScript(s *d3d9.Subsystem, mode string, keys []byte) {
	f := s.FrameCount
	switch mode {
	case "boot", "episode1", "outside":
		// Auto-advance past initial device selection -> Main Menu -> New Game once TitleFirstFrame is reached.
		if s.TitleFirstFrame > 0 && s.CubeTexCount < 8 && f >= s.TitleFirstFrame {
			df := (f - s.TitleFirstFrame) % 50
			if df >= 20 && df < 26 {
				keys[0x1C] = 0x80 // DIK_RETURN
			}
		}
		if s.CantinaFirstFrame > 0 && f >= s.CantinaFirstFrame {
			cf := f - s.CantinaFirstFrame
			if (cf >= 10 && cf < 16) || (cf >= 40 && cf < 46) {
				keys[0x1C] = 0x80 // DIK_RETURN
			}
			if mode == "episode1" && cf >= 55 {
				switch {
				case cf < 100: // Cantina: walk north
					keys[0x11] = 0x80 // DIK_W (Player 1 Up)
				case cf < 150: // Clear the pillar, line up with the Episode I archway
					keys[0x11] = 0x80
					keys[0x20] = 0x80 // DIK_D (Player 1 Right)
				case cf < 215: // Inside the Episode I room: walk left toward the Chapter 1 door
					keys[0x1E] = 0x80 // DIK_A (Player 1 Left)
				default: // Walk into the Chapter 1 (Negotiations) door
					keys[0x11] = 0x80
				}
				if cf >= 300 && (cf%40) < 6 {
					keys[0x16] = 0x80 // Tap DIK_U (Player 1 Jump / Confirm in chapter menu)
				}
			}
			if mode == "outside" && cf >= 55 {
				// Walk south around the bottom of the bar counter, left past the barista, then north through the exit to the right of the band.
				switch {
				case cf < 80: // Walk south to clear the bottom edge of the bar counter
					keys[0x1F] = 0x80 // DIK_S (Down)
				case cf < 99: // Walk left past the barista, lining up with the exit archway to the right of the band
					keys[0x1E] = 0x80 // DIK_A (Left)
				case cf < 170: // Walk straight north through the exit archway to the outside of the Cantina
					keys[0x11] = 0x80 // DIK_W (Up)
				case cf < 210: // Once outside (where the camera faces South toward the exit door), walk away from the door into the courtyard
					keys[0x1F] = 0x80 // DIK_S (Down)
				}
			}
		}
	case "loop":
		if f >= 200 {
			switch ph := (f - 200) % 120; {
			case ph < 6:
				keys[0x1C] = 0x80 // DIK_RETURN
			case ph >= 60 && ph < 66:
				keys[0x3C] = 0x80 // DIK_F2
			}
		}
	}
}
