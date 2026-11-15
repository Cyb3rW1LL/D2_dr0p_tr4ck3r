import time
import re
import os
import csv
import difflib
from datetime import datetime
import mss
import numpy as np
import cv2
import pytesseract

# ==============================================================================
# CONFIGURATION
# ==============================================================================

# 1. Path to Tesseract OCR executable (Update if installed elsewhere)
pytesseract.pytesseract.tesseract_cmd = r'C:\Program Files\Tesseract-OCR\tesseract.exe'

# 2. DUAL SCREEN REGIONS (Your calibrated coordinates preserved)
# Top-Left: Where loot filter alerts & in-game chat messages appear
DROP_REGION = {
    "top": 0,
    "left": 0,
    "width": 500,
    "height": 500
}

# Top-Right: Where the current Area / Map name appears (e.g., Cold Plains, Torajan Jungle)
ZONE_REGION = {
    "top": 0,
    "left": 1750,
    "width": 500,
    "height": 250
}

# 3. Towns where drop logging should be suppressed (selling, stash management, etc.)
TOWN_ZONES = {
    "Rogue Encampment",
    "Lut Gholein",
    "Kurast Docks",
    "Pandemonium Fortress",
    "Harrogath"
}

# 4. Loot Filter Patterns (Regex)
DROP_PATTERNS = [
    r"(?i)\b(el|eld|tir|nef|eth|ith|tal|ral|ort|thul|amn|sol|shael|dol|hel|io|lum|ko|fal|lem|pul|um|mal|ist|gul|vex|ohm|lo|sur|ber|jah|cham|zod)\s+rune\b",
    r"(?i)[oO*\-\.\s=]*worldstone\s+shard[oO*\-\.\s=]*",
    r"(?i)larzuk'?s\s+puzzlebox|puzzlebox",
    r"(?i)larzuk'?s\s+puzzle\s+piece|puzzle\s+piece",
    r"(?i)vial\s+of\s+lightsong",
    r"(?i)lilith'?s\s+mirror",
    r"(?i)key\s+of\s+(terror|hate|destruction)"
]

# 5. Complete Vanilla + Project Diablo 2 (Through Season 14 Alliance) Zone List
KNOWN_ZONES = [
    # --- Towns ---
    "Rogue Encampment", "Lut Gholein", "Kurast Docks", "Pandemonium Fortress", "Harrogath",

    # --- Vanilla Act 1 - Act 5 ---
    "Blood Moor", "Den of Evil", "Cold Plains", "Cave Level",
    "Burial Grounds", "Crypt", "Mausoleum", "Stony Field", "Tristram",
    "Underground Passage", "Dark Wood", "Black Marsh", "Hole Level",
    "Forgotten Tower", "Tower Cellar", "Tamoe Highland", "Pit Level",
    "Monastery Gate", "Outer Cloister", "Barracks", "Jail Level",
    "Inner Cloister", "Cathedral", "Catacombs", "Secret Cow Level", "Moo Moo Farm",
    "Sewers", "Rocky Waste", "Stony Tomb", "Dry Hills",
    "Halls of the Dead", "Far Oasis", "Maggot Lair", "Lost City",
    "Ancient Tunnels", "Valley of Snakes", "Claw Viper Temple", "Harem",
    "Palace Cellar", "Arcane Sanctuary", "Canyon of the Magi", "Tal Rasha's Tomb",
    "Tal Rasha's Chamber", "Spider Forest", "Arachnid Lair",
    "Spider Cavern", "Great Marsh", "Flayer Jungle", "Swampy Pit",
    "Flayer Dungeon", "Lower Kurast", "Kurast Bazaar", "Ruined Temple",
    "Disused Fane", "Upper Kurast", "Forgotten Reliquary", "Forgotten Temple",
    "Kurast Causeway", "Ruined Fane", "Disused Reliquary", "Travincal",
    "Durance of Hate", "Outer Steppes",
    "Plains of Despair", "City of the Damned", "River of Flame",
    "Chaos Sanctuary", "Bloody Foothills", "Frigid Highlands",
    "Abaddon", "Arreat Plateau", "Pit of Acheron", "Crystalline Passage",
    "Frozen River", "Glacial Trail", "Drifter Cavern", "Frozen Tundra",
    "Infernal Pit", "Ancients' Way", "Icy Cellar", "Arreat Summit",
    "Nihlathak's Temple", "Halls of Anguish", "Halls of Pain", "Halls of Vaught",
    "Worldstone Keep", "Throne of Destruction", "Worldstone Chamber",

    # --- PD2 Tier 1 Maps ---
    "Ancestral Trial", "Fall of Caldeum", "Lost Temple", "Phlegethon",
    "Poisoned Well", "Ruins of Viz-Jun", "Sanatorium", "Shadows of Westmarch",
    "Tomb of Zoltun Kulle", "Torajan Jungle",

    # --- PD2 Tier 2 Maps ---
    "Ashen Plains", "Canyon of Sescheron", "Descent From Mount Arreat",
    "Fortress of Aranoch", "Halls of Torture", "Horazon's Memory",
    "Pandemonium Citadel", "Ruined Cistern", "Sewers of Harrogath",
    "Throne of Insanity",

    # --- PD2 Tier 3 Maps ---
    "Arreat Battlefield", "Bastion Keep", "Blood Moon", "Demon Road",
    "Kehjistan Marketplace", "River of Blood",
    "Royal Crypts", "Skovos Stronghold", "Kyovoshad",

    # --- PD2 Tier 4 Dungeons ---
    "Plains of Torment", "Sanctuary of Sin", "Steppes of Daken-Shar",

    # --- PD2 Unique Maps & Event Vaults ---
    "City of Ureh", "Djinn's Domain", "Fallen Gardens", "Imperial Palace",
    "Na-Krul's Abyss", "Outer Void", "Warlord of Blood", "Zhar's Sanctum",
    "Treasure Vault", "Tainted Hive",

    # --- PD2 Uber & Pinnacle Boss Arenas ---
    "Matron's Den", "Forgotten Sands", "Furnace of Pain", "Uber Tristram",
    "Abyssal Void", "Rathma's Void", "Cathedral of Light", "Demonic Cellar",
    "Pandemonium Finale"
]

# 6. Log File & Polling Settings
LOG_FILE = "my_d2_drops.csv"
POLL_INTERVAL = 0.5      # Scans both regions every 0.5 seconds (2x per second)
COOLDOWN_SECONDS = 15    # Prevents duplicate logs while drop text fades off screen
SAVE_DEBUG_IMAGES = True # Saves 1 snapshot on startup so you can check both boxes

# ==============================================================================
# TRACKER LOGIC
# ==============================================================================

class DualRegionDropLogger:
    def __init__(self):
        self.recent_drops = {}
        self.current_zone = "Unknown Area"
        self.known_zones_lower = {z.lower(): z for z in KNOWN_ZONES}
        self.init_csv()

    def init_csv(self):
        """Creates the CSV log file with Area/Location tracking if it doesn't exist or is empty."""
        csv_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), LOG_FILE)
        needs_header = not os.path.exists(csv_path) or os.path.getsize(csv_path) == 0
        if needs_header:
            with open(csv_path, mode='w', newline='', encoding='utf-8') as f:
                writer = csv.writer(f)
                writer.writerow(["Timestamp", "Location / Area", "Matched Item", "Raw OCR Line"])
            print(f"[Info] Created new drop log file: {csv_path}")

    def preprocess_image(self, img_bgra):
        """
        Converts BGRA to HSV color space and isolates bright white text 
        plus saturated text colors (purple WSS, orange runes, gold uniques, green sets)
        so Tesseract can read colored loot filter notifications reliably.
        """
        # Drop Alpha channel if present
        if img_bgra.shape[2] == 4:
            bgr = cv2.cvtColor(img_bgra, cv2.COLOR_BGRA2BGR)
        else:
            bgr = img_bgra

        # Upscale 2x for cleaner OCR recognition
        h, w = bgr.shape[:2]
        scaled = cv2.resize(bgr, (w * 2, h * 2), interpolation=cv2.INTER_CUBIC)

        # Convert to HSV color space
        hsv = cv2.cvtColor(scaled, cv2.COLOR_BGR2HSV)

        # Mask 1: Bright White / Light Gray text (standard D2 items & UI)
        lower_white = np.array([0, 0, 160])
        upper_white = np.array([180, 50, 255])
        mask_white = cv2.inRange(hsv, lower_white, upper_white)

        # Mask 2: Saturated Colored text (Purple WSS, Orange runes, Yellow uniques, Green sets)
        lower_color = np.array([0, 80, 120])
        upper_color = np.array([180, 255, 255])
        mask_color = cv2.inRange(hsv, lower_color, upper_color)

        # Combine masks
        combined_mask = cv2.bitwise_or(mask_white, mask_color)

        # Invert so text is solid black on a white background (ideal for Tesseract)
        inverted = cv2.bitwise_not(combined_mask)
        return inverted

    def update_current_zone(self, zone_text):
        """Matches top-right OCR text strictly against known zones (with fuzzy tolerance)."""
        for line in zone_text.splitlines():
            clean = re.sub(r'[^A-Za-z\'\-\s]', '', line).strip()
            if len(clean) < 4:
                continue
            
            clean_lower = clean.lower()

            # 1. Exact substring match against KNOWN_ZONES
            for known_lower, proper_name in self.known_zones_lower.items():
                if known_lower in clean_lower:
                    if self.current_zone != proper_name:
                        self.current_zone = proper_name
                        print(f"[Area Changed] -> {self.current_zone}")
                    return

            # 2. Fuzzy match (handles minor OCR typos)
            matches = difflib.get_close_matches(clean_lower, self.known_zones_lower.keys(), n=1, cutoff=0.82)
            if matches:
                proper_name = self.known_zones_lower[matches[0]]
                if self.current_zone != proper_name:
                    self.current_zone = proper_name
                    print(f"[Area Changed] -> {self.current_zone}")
                return

    def log_drop(self, item_name, raw_line):
        """Records a valid drop along with the current area to console and CSV, skipping towns."""
        if self.current_zone in TOWN_ZONES:
            return

        # Normalize any Worldstone Shard variant to a clean standard name for CSV/display
        normalized_item = item_name.strip()
        if "worldstone shard" in normalized_item.lower():
            normalized_item = "Worldstone Shard"

        now = time.time()
        clean_key = normalized_item.lower().strip()

        if clean_key in self.recent_drops:
            if now - self.recent_drops[clean_key] < COOLDOWN_SECONDS:
                return

        self.recent_drops[clean_key] = now
        timestamp_str = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        
        print(f"\n[{timestamp_str}] DROP DETECTED!")
        print(f"  Item : {normalized_item.upper()}")
        print(f"  Area : {self.current_zone}")
        print(f"  Raw  : '{raw_line}'\n")
        
        self.init_csv()
        csv_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), LOG_FILE)
        with open(csv_path, mode='a', newline='', encoding='utf-8') as f:
            writer = csv.writer(f)
            writer.writerow([timestamp_str, self.current_zone, normalized_item, raw_line])

    def run(self):
        self.init_csv()
        print("Starting Diablo II Dual-Region Drop Logger (HSV Color Masking Active)...")
        print(f"Watching Drops (Left) : {DROP_REGION}")
        print(f"Watching Area (Right) : {ZONE_REGION}")
        print(f"Scan Rate             : Every {POLL_INTERVAL}s (Town drops ignored)")
        print(f"Logging to            : {os.path.abspath(LOG_FILE)}")
        print("Press Ctrl+C to stop.\n")

        with mss.mss() as sct:
            first_frame = True
            while True:
                # 1. Capture both regions
                drop_shot = np.array(sct.grab(DROP_REGION))
                zone_shot = np.array(sct.grab(ZONE_REGION))

                # 2. Preprocess both images using HSV color masking
                drop_proc = self.preprocess_image(drop_shot)
                zone_proc = self.preprocess_image(zone_shot)

                # Save one-time alignment snapshots on startup
                if first_frame and SAVE_DEBUG_IMAGES:
                    cv2.imwrite("debug_left_drops_raw.png", drop_shot)
                    cv2.imwrite("debug_left_drops_ocr.png", drop_proc)
                    cv2.imwrite("debug_right_zone_raw.png", zone_shot)
                    cv2.imwrite("debug_right_zone_ocr.png", zone_proc)
                    print("[Info] Saved 4 debug snapshots ('debug_left_drops_*.png' & 'debug_right_zone_*.png').")
                    print("[Info] Check both '*_ocr.png' images to verify your colored loot filter text is crisp black on white!\n")
                    first_frame = False

                # 3. Read Top-Right Zone first so any drop on this frame has the latest Area
                zone_text = pytesseract.image_to_string(zone_proc, config='--psm 6')
                self.update_current_zone(zone_text)

                # 4. Read Top-Left Drop Messages
                drop_text = pytesseract.image_to_string(drop_proc, config='--psm 6')
                for line in drop_text.splitlines():
                    clean_line = line.strip()
                    if not clean_line:
                        continue

                    for pattern in DROP_PATTERNS:
                        match = re.search(pattern, clean_line)
                        if match:
                            self.log_drop(match.group(0), clean_line)

                time.sleep(POLL_INTERVAL)

if __name__ == "__main__":
    logger = DualRegionDropLogger()
    try:
        logger.run()
    except KeyboardInterrupt:
        print("\nLogger stopped.")
