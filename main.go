package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"mime/multipart"
	"net/http"
	_ "net/http/pprof"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"

	"github.com/vladimirvivien/go4vl/device"
	"github.com/vladimirvivien/go4vl/v4l2"
)

type ClientChan chan []byte

var (
	frames       <-chan []byte
	cameraDevice *device.Device
	devName      = "/dev/video99"
	clients      = make(map[ClientChan]struct{})
	clientsMutex sync.Mutex
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

// Broadcast frames to another channel for all incoming clients to use
func frameBroadcaster() {
	// Get raw frames from the camera (these frames should be MJPEG images)
	frames := cameraDevice.GetOutput()
	for frame := range frames {
		// Check if the frame is empty or invalid
		if len(frame) == 0 {
			log.Println("Received empty frame, skipping...")
			continue
		}
		// Send the raw frame to the global channel for clients
		clientsMutex.Lock()
		for clientChan := range clients {
			select {
			case clientChan <- frame:
			default:
				// Drop frame
			}
		}
		clientsMutex.Unlock()
	}
}

func resetCameraWeb(w http.ResponseWriter, req *http.Request) {
	fmt.Println("Restarting camera")
	restartCamera()
	fmt.Fprint(w, "Camera restarted.")
}

// Serve the stream of frames to the client
func imageServ(w http.ResponseWriter, req *http.Request) {
	fmt.Println("Client connected", req.RemoteAddr)
	clientChan := make(ClientChan, 30) // Per-client buffer
	clientsMutex.Lock()
	clients[clientChan] = struct{}{}
	clientsMutex.Unlock()

	defer func() {
		clientsMutex.Lock()
		delete(clients, clientChan)
		clientsMutex.Unlock()
		close(clientChan)
		fmt.Println("Client disconnected", req.RemoteAddr)
	}()

	mimeWriter := multipart.NewWriter(w)
	defer mimeWriter.Close()

	w.Header().Set("Content-Type", fmt.Sprintf("multipart/x-mixed-replace; boundary=%s", mimeWriter.Boundary()))

	partHeader := make(textproto.MIMEHeader)
	partHeader.Add("Content-Type", "image/jpeg")

	for {
		select {
		case frame, ok := <-clientChan:
			if !ok {
				return
			}

			part, err := mimeWriter.CreatePart(partHeader)
			if err != nil {
				log.Printf("CreatePart failed: %v", err)
				return
			}

			if _, err := part.Write(frame); err != nil {
				log.Printf("Write failed: %v", err)
				return
			}
		case <-req.Context().Done():
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
	http.HandleFunc("/restart", resetCameraWeb)

	go frameBroadcaster()
	// go func() {
	// 	log.Println("Starting pprof server on :6060")
	// 	log.Println(http.ListenAndServe(":6060", nil))
	// }()

	log.Fatal(http.ListenAndServe(port, nil))
}
