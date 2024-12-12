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
	"os/exec"
	"path/filepath"
	"time"

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

// Broadcast frames to another channel for all incoming clients to use
func frameBroadcaster() {
	// Start the FFmpeg subprocess to write to an MKV file with H.264 compression and segmentation
	cmd := exec.Command(
		"ffmpeg",
		"-loglevel", "debug", // Enable debug level logging for FFmpeg
		"-y",          // Overwrite output file if it exists
		"-f", "mjpeg", // MJPEG format (because frames are JPEG images)
		"-framerate", "15",
		"-i", "pipe:0", // Read input from stdin (pipe)
		"-vf", "drawtext=text='%{localtime}':fontcolor=white:fontsize=24:x=10:y=10",
		"-c:v", "h264_v4l2m2m", // H.264 encoding for output // Pixel format for output video
		"-crf", "0", // Lossless quality (zero compression)
		"-pix_fmt", "yuv420p",
		"-b:v", "1M", // Bitrate for video encoding
		"-f", "segment",
		"-r", "15", // Force framerate
		"-reset_timestamps", "1",
		"-use_wallclock_as_timestamps", "1",
		"-segment_time", "1800", // Segment duration (30 minutes)
		"-segment_format", "mkv", // MKV format for segmented files
		"-segment_atclocktime", "1", // Reset timestamps at each segment
		"-strftime", "1",
		"-vsync", "2",
		"clips/compressed_%Y%m%dT%H%M%S.mkv",
	)

	// Create a pipe for sending raw MJPEG frames to FFmpeg
	ffmpegIn, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("Failed to create FFmpeg stdin pipe: %s", err)
	}

	// Start the FFmpeg process
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start FFmpeg process: %s", err)
	}

	// Close ffmpegIn and wait for the command to finish when done
	defer func() {
		ffmpegIn.Close() // Close the pipe when done
		cmd.Wait()       // Wait for FFmpeg to finish
	}()

	// Get raw frames from the camera (these frames should be MJPEG images)
	frames := cameraDevice.GetOutput()
	for frame := range frames {
		// Check if the frame is empty or invalid
		if len(frame) == 0 {
			log.Println("Received empty frame, skipping...")
			continue
		}

		// Write the raw MJPEG frame (JPEG image) to FFmpeg's stdin
		_, err = ffmpegIn.Write(frame)
		if err != nil {
			log.Printf("Failed to write frame to FFmpeg: %s", err)
			return // Exit if writing to FFmpeg fails
		}

		// Optionally, send the raw frame to the global channel for clients
		select {
		case encodedFrameChan <- frame:
		default:
			log.Println("Frame channel full, dropping frame to keep up with the camera.")
		}

		// Reset camera every 30 Minutes 1-2 times to try and remove the obscure lag
		currTime := time.Now()
		minute := currTime.Minute()
		second := currTime.Second()
		if minute == 30 || minute == 0 {
			if second >= 0 && second <= 2 {
				fmt.Println("Restarting Camera...")
				restartCamera()
			}
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
	// go func() {
	// 	log.Println("Starting pprof server on :6060")
	// 	log.Println(http.ListenAndServe(":6060", nil))
	// }()

	log.Fatal(http.ListenAndServe(port, nil))
}
