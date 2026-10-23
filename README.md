# q2-filebrowser

A file manager for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod), after KOReader's Filebrowser plugin. Turn it on from the Q2 and it shows a QR code. Scan it with your phone, or open the address on a computer and type the PIN, on the same Wi-Fi, to browse the card, upload files and whole folders, download (a folder or several files as a ZIP), play songs, view pictures and text, and rename, move, delete and make folders. The page is built in: nothing is fetched from the internet. It needs a Q2 Pod build that supports card apps (its **System settings → Apps** lists File Browser).

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **File Browser**.

**By hand:**

1. Download `q2-filebrowser-*.zip` from the [latest release](https://github.com/group-a-study/q2-filebrowser/releases/latest).
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.filebrowser/q2-filebrowser`, `.filebrowser/run` and `.filebrowser/q2app`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

File Browser then has a row in **Streaming**.

## Use

1. On the Q2, open **Streaming → File Browser**. It turns on, and once it answers, the QR code page opens: the code, the address (such as `192.168.1.23:8000`) and a PIN.
2. Scan the code with your phone's camera, which opens the page signed in. On a computer, open the address and type the PIN.
3. **Back** shows File Browser's page on the Q2: **Open on Your Phone** (the address, PIN and code again), the card's free space or what is moving, **Read Only**, and **Turn Off File Browser**.

The key in the QR code and the PIN are made once and kept, so a phone stays signed in from one time to the next; ten wrong PINs lock the PIN for a minute. **Read Only** lets the browser look and download, nothing else. The Q2 stays awake while files move, and for five minutes after it was turned on or the page was last used; the screen still turns off. Unused for 15 minutes, File Browser turns itself off. Uploads land in the folder the browser shows; run **Library → Update Local Music** to play new music.

**The page:** a folder lists its folders, then its files, by name, date or size, with a breadcrumb back up. A press opens a folder or plays a song; **⋯** on a row offers play, show (a picture, or a text file such as a `.cue`, `.lrc` or EQ preset), download (a folder as a ZIP), rename, move and delete. **Select** picks several for a ZIP, a move or a delete. **Upload** sends files or a whole folder, one file at a time with its progress; on a computer, dropping files or folders on the page does the same. A name already there asks first: replace, or keep both. **Search** finds names from the folder down. Hidden files (names starting with a dot, such as the apps' folders) show only when turned on in Options.

## Safety

- Every path is checked against the card's root, so nothing outside the card can be read or written, and the card itself can't be renamed, moved or deleted.
- The page answers only a browser signed in with the key or the PIN, held in an `HttpOnly`, `SameSite=Strict` cookie, and every change must carry the header `X-Q2: 1`.
- A file is sent as itself, never run: sandboxed (`Content-Security-Policy: sandbox`) and `nosniff`, and an HTML, XML or script file is sent as plain bytes.
- Names a FAT card can't hold are refused for a new name and replaced in an upload's. An upload that would leave less than 16 MB free is refused.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace `.filebrowser/q2-filebrowser`, `.filebrowser/run` and `.filebrowser/q2app`. Either way `.filebrowser/config.json` (the key, PIN, Read Only and idle time) is kept. Delete `web_key` and `pin` from it for new ones.

To remove it, press **File Browser** twice in **System settings → Apps**, or delete `.filebrowser`.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.filebrowser` and the release ZIP beside them. Pushing a tag `q2-vX.Y` that matches `cmd/q2-filebrowser/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). To look at the page without a Q2, serve any folder as the card:

    Q2_DEMO=/some/folder go test -run TestDemo -timeout 0 -v ./cmd/q2-filebrowser

How it works: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). The QR encoder (`internal/qr`) follows Project Nayuki's QR Code generator (MIT).
