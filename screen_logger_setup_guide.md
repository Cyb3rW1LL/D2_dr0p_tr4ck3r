# Diablo II Real-Time Screen Drop Logger

This tool watches your in-game message log using Optical Character Recognition (OCR) and logs Rune and Worldstone Shard notifications to a CSV file.

## 1. Install Tesseract OCR (Required Engine)
`pytesseract` is just a Python wrapper; you need the actual OCR engine installed on your system:
1. **Windows:** Download the installer from [UB-Mannheim Tesseract Wiki](https://github.com/UB-Mannheim/tesseract/wiki).
2. Install it to the default path: `C:\Program Files\Tesseract-OCR\tesseract.exe`.
3. *(Mac/Linux users: install via `brew install tesseract` or `sudo apt install tesseract-ocr` and comment out line 13 in the Python script).*

## 2. Install Python Libraries
Run the following in your terminal:
```bash
pip install -r requirements.txt
```

## 3. Configure Your Loot Filter & Screen Box
* **Window Mode:** Diablo II must be running in **Windowed** or **Borderless Windowed** mode (Fullscreen Exclusive blocks screen capture tools like `mss`).
* **Loot Filter Tip:** Edit your `.filter` file so drop notifications include a unique prefix (e.g., `[DROP] Ber Rune`). You can then add `r"\[DROP\].*"` to `DROP_PATTERNS` in the script for 100% accuracy!
* **Aligning the Capture Box:** 
  1. Start the game and trigger a test message (or open your message log).
  2. Run `python d2_screen_logger.py`.
  3. Open the newly generated `debug_raw_capture.png` image.
  4. Adjust `top`, `left`, `width`, and `height` in `MONITOR_REGION` until the image tightly frames your message block.

## 4. Go Binary Compilation
1. Cd into your target file directory where your go.mod and main.go live along with your d2_screen_logger.py live.
2. Type `go build -ldflags="-H windowsgui" -o D2DropManager.exe` in powershell or otherwise.
3. Double click the new `D2DropManager` binary.

## 5. Why Stash/Inventory Won't Trigger False Positives
Because the script *only* captures the small `MONITOR_REGION` rectangle at the top of your screen, opening your stash, cube, or inventory will never trigger fake drop logs.

## 6. Once running, if you need to stop or restart the binary:
1. In your terminal `taskkill /F /IM D2DropManager.exe 2>$null`
2. Make edits or changes.
3. Recompile the new `go.main` binary.
4. `.\D2DropManager.exe` to run it.
