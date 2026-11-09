# q2-filebrowser

A file manager for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod), after KOReader's Filebrowser plugin. Turn it on from the Q2 and it shows a QR code. Scan it with your phone, or open the address on a computer and type the PIN, on the same Wi-Fi, to browse the card, upload files and whole folders, download (a folder or several files as a ZIP), play songs, view pictures and text, rename, move, delete and make folders, and edit songs' tags and covers. Tags can be edited on the Q2 itself too. The page is built in: nothing is fetched from the internet. It needs a Q2 Pod build that supports card apps (its **System settings → Apps** lists File Browser).

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **File Browser**.

**By hand:**

1. Download `q2-filebrowser-*.zip` from the [latest release](https://github.com/group-a-study/q2-filebrowser/releases/latest).
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.filebrowser/q2-filebrowser`, `.filebrowser/run`, `.filebrowser/q2app` and its icons `.filebrowser/icon.png` and `.filebrowser/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

File Browser then has a row in **Streaming**, with its logo.

## Use

1. On the Q2, open **Streaming → File Browser**. It turns on, and once it answers, the QR code page opens: the code, the address (such as `192.168.1.23:8000`) and a PIN.
2. Scan the code with your phone's camera, which opens the page signed in. On a computer, open the address and type the PIN.
3. **Back** shows File Browser's page on the Q2: **Open on Your Phone** (the address, PIN and code again), **Edit Tags**, the card's free space or what is moving, **Read Only**, and **Turn Off File Browser**.

The key in the QR code and the PIN are made once and kept, so a phone stays signed in from one time to the next; ten wrong PINs lock the PIN for a minute. **Read Only** lets the browser look and download, nothing else. The Q2 stays awake while files move, and for five minutes after it was turned on or the page was last used; the screen still turns off. Unused for 15 minutes, File Browser turns itself off. Uploads land in the folder the browser shows; run **Library → Update Local Music** to play new music.

**The page:** a folder lists its folders, then its files, by name, date or size, with a breadcrumb back up. A press opens a folder or plays a song; **⋯** on a row offers play, show (a picture, or a text file such as a `.cue`, `.lrc` or EQ preset), download (a folder as a ZIP), rename, move and delete. **Select** picks several for a ZIP, a move or a delete. On a computer, Shift-click picks every row from the last one clicked, Ctrl- or Cmd-click picks a row without pressing Select first, Ctrl+A (Cmd+A) picks the whole folder and Escape lets them go. **Upload** sends files or a whole folder, one file at a time with its progress; on a computer, dropping files or folders on the page does the same. A name already there asks first: replace, or keep both. **Search** finds names from the folder down. Hidden files (names starting with a dot, such as the apps' folders) show only when turned on in Options.

## Tags

The Q2's library, Now Playing and Coverflow show what a song's tags say. File Browser changes them in MP3, MP2 and AAC (ID3v2), FLAC, Ogg Vorbis, Opus, M4A and M4B, AIFF, WAV, DSF and APE files: title, artist, album, album artist, track and disc (and of how many), year, genre, composer, comment, lyrics and the cover.

**On the phone or computer:** **⋯** on a song offers **Edit tags**, **⋯** on a folder **Edit tags of its songs**, and **Select** has **Tags** for the songs picked. With several songs, a field they share shows its value and a field that differs shows *Keep each song's*: only what you type in changes, so an album's songs can take one album artist, year or cover at once. **Number tracks 1–N by name** and **Titles from file names** help with a folder of untagged files. A cover can be any picture: the page makes it a JPEG of at most 1000 px, which the Q2 shows everywhere, and can save it as `cover.jpg` in the folder too. Escape, or a tap outside, asks before leaving edits unsaved.

**On the Q2:** **Edit Tags** lists the card's folders and songs; at the top are the songs playing just now, **Find a Song** and **Update Library**. A song lists its fields; a field offers what needs no typing (**Clear**, **From the File Name**, **Same as Artist**, its place in the folder for Track, the values other songs in the folder have) and **Type a New …**, which opens the Q2's keyboard. What you type shows what saving it would change, with **Save**, and for a field an album shares, **Save for All N Songs in This Folder**. The Q2's keyboard has no accents and no `&`, and takes up to 63 bytes: for those, use the phone.

**Then update the library:** the Q2 reads tags when **Library → Update Local Music** runs. The page's **Update library** button and the Q2's **Update Library** row ask Q2 Pod to run it once nothing is playing (Q2 Pod 1.5.9 and later; with an older one, run it by hand). Songs played from a folder show their new tags at once. A new cover shows in Coverflow after **System settings → Display → Coverflow → Refresh Coverflow**. Favourites and playlists keep the tags a song had when it was added.

A tag that fits the room the file keeps for tags is written in place; otherwise the file is written again beside itself (it needs that much free space, plus 16 MB) and put in its place, so a song that is playing plays on. The audio is never changed, nor is anything else in the file: MP3 gapless and ReplayGain frames, FLAC's seek table and MQA blocks, MP4 chapters. An MP3's old ID3v1 tag is kept in step, so a cleared field can't show through from it.

## Safety

- Every path is checked against the card's root, so nothing outside the card can be read or written, and the card itself can't be renamed, moved or deleted.
- The page answers only a browser signed in with the key or the PIN, held in an `HttpOnly`, `SameSite=Strict` cookie, and every change must carry the header `X-Q2: 1`.
- A file is sent as itself, never run: sandboxed (`Content-Security-Policy: sandbox`) and `nosniff`, and an HTML, XML or script file is sent as plain bytes.
- Names a FAT card can't hold are refused for a new name and replaced in an upload's. An upload that would leave less than 16 MB free is refused.
- Tags are written only to songs on the card, and **Read Only** refuses them too. A cover must be a JPEG or PNG of at most 4 MB, and the cover the page serves is sandboxed as a file is.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace `.filebrowser/q2-filebrowser`, `.filebrowser/run`, `.filebrowser/q2app`, `.filebrowser/icon.png` and `.filebrowser/icon-40.png`. Either way `.filebrowser/config.json` (the key, PIN, Read Only and idle time) is kept. Delete `web_key` and `pin` from it for new ones.

To remove it, press **File Browser** twice in **System settings → Apps**, or delete `.filebrowser`.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.filebrowser` and the release ZIP beside them. Pushing a tag `q2-vX.Y` that matches `cmd/q2-filebrowser/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). To look at the page without a Q2, serve any folder as the card:

    Q2_DEMO=/some/folder go test -run TestDemo -timeout 0 -v ./cmd/q2-filebrowser

How it works: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). The QR encoder (`internal/qr`) follows Project Nayuki's QR Code generator (MIT).
