# transcriber

## Authentication

* Create a project with the [Google Cloud Console][cloud-console], and enable
  the [Speech API][speech-api].
* From the Cloud Console, create a service account,
  download its json credentials file, then set the 
  `GOOGLE_APPLICATION_CREDENTIALS` environment variable:

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your-project-credentials.json
```

[cloud-console]: https://console.cloud.google.com

## Run realtime transcription

### Get input device name

```bash
gst-device-monitor-1.0 Audio/Source
```
### Live from mic

```bash
export TRANSCRIBER_DEVICE=alsa_input.usb-Sennheiser_Communications_Sennheiser_USB_headset-00.analog-mono

gst-launch-1.0 -v pulsesrc device=$TRANSCRIBER_DEVICE ! audioconvert ! audioresample ! audio/x-raw,channels=1,rate=16000 ! filesink location=/dev/stdout | go run transcriber.go
```

### Pipe from Chrome

```bash
pacat -r -n "Chrome input" | gst-launch-1.0 -v fdsrc ! rawaudioparse ! audioconvert ! audioresample ! audio/x-raw,channels=1,rate=16000 ! filesink location=/dev/stdout | go run transcriber.go
```

## Check log

```bash
tail -f out.log
```
