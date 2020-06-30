package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	speech "cloud.google.com/go/speech/apiv1p1beta1"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1p1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	flushTimeout     = flag.Duration("flushTimeout", 5*time.Second, "Emit any pending transcriptions after this time")
	pendingWordCount = flag.Int("pendingWordCount", 5, "Treat last N transcribed words as pending")
	sampleRate       = flag.Int("sampleRate", 16000, "Sample rate (Hz)")
	channels         = flag.Int("channels", 1, "Number of audio channels")
	lang             = flag.String("lang", "en-US", "Transcription language code")
	phrases          = flag.String("phrases", "'some','english','hint'", "Comma-separated list of phrase hints for Speech API.")
	logFile          = flag.String("logFile", "out.log", "Log file ")

	latestTranscript string
	lastIndex        = 0
	pending          []string
)

func main() {
	flag.Parse()
	file := initLogFile(*logFile)
	defer file.Close()

	speechClient, err := speech.NewClient(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	contextPhrases := []string{}
	if *phrases != "" {
		contextPhrases = strings.Split(*phrases, ",")
		log.Printf("Supplying %d phrase hints: %+q", len(contextPhrases), contextPhrases)
	}
	streamingConfig := speechpb.StreamingRecognitionConfig{
		Config: &speechpb.RecognitionConfig{
			Encoding:                   speechpb.RecognitionConfig_LINEAR16,
			SampleRateHertz:            int32(*sampleRate),
			AudioChannelCount:          int32(*channels),
			LanguageCode:               *lang,
			EnableAutomaticPunctuation: true,
			SpeechContexts: []*speechpb.SpeechContext{
				{Phrases: contextPhrases},
			},
		},
		InterimResults: true,
	}

	sendAudio(context.Background(), speechClient, streamingConfig)
}

// Consumes Stdin audio data and sends to Speech.
func sendAudio(ctx context.Context, speechClient *speech.Client, config speechpb.StreamingRecognitionConfig) {
	var stream speechpb.Speech_StreamingRecognizeClient
	receiveChan := make(chan bool)

	for {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled, exiting sender loop")
			return
		case _, ok := <-receiveChan:
			if !ok && stream != nil {
				log.Printf("Receive channel closed, resetting stream")
				stream = nil
				receiveChan = make(chan bool)
				resetIndex()
				continue
			}
		default:
			// Process audio
		}

		// Establish bi-directional connection to Cloud Speech and
		// start a go routine to listen for responses
		if stream == nil {
			stream = initStreamingRequest(ctx, speechClient, config)
			go receiveResponses(stream, receiveChan)
			log.Printf("[NEW STREAM]")
		}

		// Pipe stdin to the API.
		buf := make([]byte, 1024)
		n, err := os.Stdin.Read(buf)
		if err != nil {
			log.Printf("Could not read from stdin: %v", err)
			continue
		}

		if n > 0 {
			// Send audio, transcription responses received asynchronously
			sendErr := stream.Send(&speechpb.StreamingRecognizeRequest{
				StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
					AudioContent: buf[:n],
				},
			})
			if sendErr != nil {
				// Expected - if stream has been closed (e.g. timeout)
				if sendErr == io.EOF {
					continue
				}
				log.Printf("Could not send audio: %v", sendErr)
			}
		}

	} // for
}

// Consumes StreamingResponses from Speech.
// call stream.Recv()
func receiveResponses(stream speechpb.Speech_StreamingRecognizeClient, receiveChan chan bool) {
	// Indicate that we're no longer listening for responses
	defer close(receiveChan)

	// If no results received from Speech for some period, emit any pending transcriptions
	timer := time.NewTimer(*flushTimeout)
	go func() {
		<-timer.C
		flush()
	}()
	defer timer.Stop()

	// Consume streaming responses from Speech
	for {
		resp, err := stream.Recv()
		if err != nil {
			// Context cancelled - expected
			if status.Code(err) == codes.Canceled {
				return
			}
			log.Printf("Cannot stream results: %v", err)
			return
		}
		if err := resp.Error; err != nil {
			// Timeout - expected, when no audio sent for a time
			// or The current maximum duration (streamingLimit) ~5 minutes is reached
			if status.FromProto(err).Code() == codes.OutOfRange {
				log.Printf("Timeout from API; closing connection")
				return
			}
			log.Printf("Could not recognize: %v", err)
			return
		}
		// If nothing received for a time, stop receiving
		if !timer.Stop() {
			return
		}
		// Ok, we have a valid response from Speech.
		timer.Reset(*flushTimeout)
		processResponses(*resp)
	}
}

// Handles transcription results.
func processResponses(resp speechpb.StreamingRecognizeResponse) {
	if len(resp.Results) == 0 {
		return
	}
	result := resp.Results[0]
	alternative := result.Alternatives[0]
	latestTranscript = alternative.Transcript
	elements := strings.Split(alternative.Transcript, " ")
	length := len(elements)

	// Speech will not further update this transcription; output it
	if result.GetIsFinal() || alternative.GetConfidence() > 0 {
		final := elements[lastIndex:]
		emit(strings.Join(final, " "))
		resetIndex()
		return
	}

	// Unstable, speculative transcriptions (very likley to change)
	unstable := ""
	if len(resp.Results) > 1 {
		unstable = resp.Results[1].Alternatives[0].Transcript
	}
	// Treat last N words as pending.
	// Treat delta between last and current index as steady
	if length < *pendingWordCount {
		lastIndex = 0
		pending = elements
		emitStages([]string{}, pending, unstable)
	} else if lastIndex < length-*pendingWordCount {
		steady := elements[lastIndex:(length - *pendingWordCount)]
		lastIndex += len(steady)
		pending = elements[lastIndex:]
		emitStages(steady, pending, unstable)
	}
}

// initStreamingRequest Send the initial configuration message
// https://pkg.go.dev/cloud.google.com/go@v0.60.0/speech/apiv1p1beta1?tab=doc#Client.StreamingRecognize
func initStreamingRequest(ctx context.Context, client *speech.Client, config speechpb.StreamingRecognitionConfig) speechpb.Speech_StreamingRecognizeClient {
	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		log.Fatal(err)
	}
	// Send the initial configuration message.
	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &config,
		},
	}); err != nil {
		log.Printf("Error sending initial config message: %v", err)
		return nil
	}
	log.Printf("Initialised new connection to Speech API")
	return stream
}

func initLogFile(logFile string) *os.File {
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open log file", logFile, ":", err)
	}
	log.SetOutput(file)
	log.Printf("Set logfile %s", logFile)
	return file
}

func emitStages(steady []string, pending []string, unstable string) {
	// Concatenate the different segments
	// msg := fmt.Sprintf("%s|%s|%s", strings.Join(steady, " "),
	// 	strings.Join(pending, " "), unstable)

	msg := fmt.Sprintf("%s", strings.Join(steady, " ")+" ")
	emit(msg)
}

func emit(msg string) {
	fmt.Print(msg)
}

func flush() {
	// log.Printf("flush()...")
	msg := ""
	if pending != nil {
		msg += strings.Join(pending, " ")
	}
	if msg != "" {
		log.Printf("Flushing...")
		emit(msg)
	}
	resetIndex()
}

func resetIndex() {
	// log.Printf("resetIndex()...")
	lastIndex = 0
	pending = nil
}
