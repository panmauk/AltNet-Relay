AltNet Relay for Mac
====================

This folder builds AltNet Relay on your Mac and makes an installer (.dmg).

1. Double-click "Build AltNet Relay.command".

   The first time, macOS may say it can't be opened because it is from an
   unidentified developer. Then: right-click (or Control-click) the file,
   choose Open, and click Open.

2. A Terminal window opens and does the rest. Two things may need you:
   - If Apple's Command Line Tools aren't installed, Apple's own window
     appears: click Install and agree. It takes a few minutes.
   - The Mac must be on the internet the first time (to download Go,
     about 65 MB).

3. When it says "Done", a Finder window shows "AltNet Relay <version>.dmg".
   Open it and drag AltNet Relay onto Applications.

4. Open AltNet Relay from Applications. The first time, macOS asks whether
   it may use notifications (say Allow), and the first call asks for the
   microphone (and camera, for video).

Needs: macOS 12 (Monterey) or newer -- Ventura is fine -- and about 1 GB of
free space while building. Nothing is installed outside this folder except
the Command Line Tools; you can delete the folder afterwards.

Running it again is quicker: Go is already downloaded.

If something goes wrong, the full output is in build.log in this folder.
