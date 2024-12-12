package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"

	"bytes"

	"github.com/pixiv/go-libjpeg/jpeg"
	"github.com/vladimirvivien/go4vl/device"
	"github.com/vladimirvivien/go4vl/v4l2"
)

var (
	frames           <-chan []byte
	cameraDevice     *device.Device
	devName          = "/dev/video99"
	encodedFrameChan = make(chan []byte, 10)
)

// setupCamera initializes the camera device and starts the stream.
func setupCamera() (*device.Device, error) {
	camera, err := device.Open(
		devName,
		device.WithPixFormat(v4l2.PixFormat{PixelFormat: v4l2.PixelFmtMJPEG, Width: 1280, Height: 720}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to open device: %w", err)
	}

	if err := camera.Start(context.TODO()); err != nil {
		camera.Close()
		return nil, fmt.Errorf("camera start: %w", err)
	}

	return camera, nil
}

// Compress the incoming raw frames into another channel that's used to send to
// the clients.
func frameBroadcaster() {
	frames := cameraDevice.GetOutput()
	for frame := range frames {
		img, err := jpeg.Decode(bytes.NewReader(frame), &jpeg.DecoderOptions{})
		if err != nil {
			log.Printf("failed to decode MJPEG frame: %s", err)
			restartCamera()
			continue
		}

		// Compress the raw mjpeg frame
		var compressedFrame bytes.Buffer
		compressionOptions := &jpeg.EncoderOptions{Quality: 50}
		err = jpeg.Encode(&compressedFrame, img, compressionOptions)
		if err != nil {
			log.Printf("failed to encode jpeg frame: %s", err)
			restartCamera()
			continue
		}

		// Send the compressed frame to the global channel
		select {
		case encodedFrameChan <- compressedFrame.Bytes():
		default:
			log.Println("Frame channel full, dropping frame to keep up with the camera.")
		}
	}
}

// Serve the stream of frames to the client
func imageServ(w http.ResponseWriter, req *http.Request) {
	mimeWriter := multipart.NewWriter(w)
	w.Header().Set("Content-Type", fmt.Sprintf("multipart/x-mixed-replace; boundary=%s", mimeWriter.Boundary()))
	defer mimeWriter.Close()

	partHeader := make(textproto.MIMEHeader)
	partHeader.Add("Content-Type", "image/jpeg")

	for frame := range encodedFrameChan {
		partWriter, err := mimeWriter.CreatePart(partHeader)
		if err != nil {
			log.Printf("failed to create multi-part writer: %s", err)
			return
		}

		if _, err := partWriter.Write(frame); err != nil {
			log.Printf("failed to write compressed image: %s", err)
			return
		}
	}
}

// restartCamera stops and reopens the camera device.
func restartCamera() {
	if cameraDevice != nil {
		cameraDevice.Close()
	}
	var err error
	cameraDevice, err = setupCamera()
	if err != nil {
		log.Printf("failed to restart camera: %s", err)
	}
}

var videoDir = "/home/elff/webcam-sv/mycode/clips" // Directory containing video files

// listVideosHandler lists all .mkv files in the video directory and provides download links.
func listVideosHandler(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir(videoDir)
	if err != nil {
		http.Error(w, "Unable to read directory", http.StatusInternalServerError)
		return
	}

	var videoFiles []string
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".mkv" || filepath.Ext(file.Name()) == ".zip" {
			videoFiles = append(videoFiles, file.Name())
		}
	}

	// Define the HTML template for listing files
	const tpl = `
	<!DOCTYPE html>
	<html>
	<head>
		<title>Video List</title>
	</head>
	<body>
		<h1>Available Videos</h1>
		<table border="1">
			<tr>
				<th>Filename</th>
				<th>Action</th>
			</tr>
			{{range .}}
			<tr>
				<td>{{.}}</td>
				<td><a href="/download/{{.}}">Download</a></td>
			</tr>
			{{end}}
		</table>
	</body>
	</html>
	`

	tmpl, err := template.New("videoList").Parse(tpl)
	if err != nil {
		http.Error(w, "Unable to parse template", http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, videoFiles)
	if err != nil {
		http.Error(w, "Unable to execute template", http.StatusInternalServerError)
		return
	}
}

// downloadHandler serves video files for download.
func downloadHandler(w http.ResponseWriter, r *http.Request) {
	fileName := r.URL.Path[len("/download/"):]

	filePath := filepath.Join(videoDir, fileName)
	http.ServeFile(w, r, filePath)
}

func main() {
	port := ":8080"
	flag.StringVar(&port, "p", port, "webcam service port")
	flag.Parse()

	var err error
	cameraDevice, err = setupCamera()
	if err != nil {
		log.Fatalf("failed to initialize camera: %s", err)
	}
	defer cameraDevice.Close()

	log.Printf("Serving images on [%s/stream]", port)
	http.HandleFunc("/stream", imageServ)
	http.HandleFunc("/videos", listVideosHandler)
	http.HandleFunc("/download/", downloadHandler)

	go frameBroadcaster()

	log.Fatal(http.ListenAndServe(port, nil))
}
