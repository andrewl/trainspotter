package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-stomp/stomp/v3"
	"github.com/icza/mjpeg"
	"github.com/spf13/viper"
	"io/ioutil"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/vladimirvivien/go4vl/device"
)

// PassingTrain is a struct to hold information about a passing train
type PassingTrain struct {
	Headcode                 string
	Origin                   string
	Destination              string
	Operator                 string
	Category                 string
	PowerType                string
	TimingLoad               string
	OperatingCharacteristics string
	Speed                    string
	DateTime                 string
	FrameCount               int
	HasPassed                bool
}

// getConfigValue is a function to get a value from the config file
func getConfigValue(key string) (value string) {
	value, _ = viper.Get(key).(string)
	return value
}

// Global variable to hold the video capture device
var videoCaptureDevice *device.Device

// Global variable to hold the MQTT client
var mqttClient mqtt.Client

// Global variable to hold the current passing trains
var currentPassingTrains []PassingTrain

// Global logger variable
var logger *slog.Logger

var berths []string

func main() {

	//set some default configuration
	viper.SetDefault("stomp_url", "publicdatafeeds.networkrail.co.uk:61618")
	viper.SetDefault("mqtt_url", "")
	viper.SetDefault("mqtt_topic", "")
	viper.SetDefault("log_filename", "")
	viper.SetDefault("ukra_api", "")
	viper.SetDefault("save_dir", "/tmp/")

	//load in config
	viper.SetConfigName("config")
	viper.AddConfigPath("/etc/trainspotter")
	viper.AddConfigPath("$HOME/.trainspotter")
	viper.AddConfigPath(".")
	err := viper.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("Fatal error config file: %w", err))
	}

	//we need to have an area ID set
	if getConfigValue("area_id") == "" {
		panic("Area ID not set")
	}

	//split the berths into an array
	berths = viper.GetStringSlice("berths")

	fmt.Printf("berths: %v\n", berths)

	var logOutput *os.File
	if getConfigValue("log_filename") != "" {

		logOutput, err = os.OpenFile(getConfigValue("log_filename"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			panic("Error opening log file: " + err.Error())
		}

		defer logOutput.Close()

	} else {

		logOutput = os.Stderr

	}

	logHandler := slog.NewTextHandler(logOutput, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	})

	logger = slog.New(logHandler)

	if getConfigValue("mqtt_url") != "" {

		opts := mqtt.NewClientOptions()
		opts.AddBroker(getConfigValue("mqtt_url"))
		opts.SetClientID("trainspotter")
		mqttClient = mqtt.NewClient(opts)
		if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
			panic(token.Error())
		}

		defer mqttClient.Disconnect(250)
	}

	err = errors.New("")

	// open device
	videoCaptureDevice, err = device.Open(getConfigValue("video_capture_device"))
	if err != nil {
		log.Fatalf("failed to open device: %s", err)
	}
	defer videoCaptureDevice.Close()

	// start stream
	ctx, stop := context.WithCancel(context.TODO())
	if err := videoCaptureDevice.Start(ctx); err != nil {
		log.Fatalf("failed to start stream: %s", err)
	}

	currentPassingTrains = []PassingTrain{}

	go recordTrains()

	for {
		err := listenForTrains()
		if err != nil {
			fmt.Printf("%+v\n", err)
		}
	}

	stop()
}

// SFMsg is a struct to hold the data from a SF message
// S-Class train describer messages provide updates relating to the status of signalling equipment within a train describer area. Not all TD areas provide this information.
// SF Messages Updates the signalling data for a specified set of signalling elements
// See also https://wiki.openraildata.com/index.php?title=S-Class_Train_Describer_(TD)
type SFMsg struct {
	MsgType string `json:"msg_type"`
	AreaID  string `json:"area_id"`
	Time    string `json:"time"`
	Address string `json:"address"`
	Data    string `json:"data"`
}

// CMsg is a struct to hold the data from a C message
// C-Class train describer messages provide updates relating to the stepping of train descriptions between TD berths.
// There are four types of C message: CA, CB, CC and CT
type CMsg struct {
	MsgType string `json:"msg_type"`
	AreaID  string `json:"area_id"`
	Time    string `json:"time"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Descr   string `json:"descr"`
}

// Message is a struct to hold the message data - it will contain either a SFMsg or a CAMsg, CBMsg or CCMsg
type Message struct {
	SFMsg *SFMsg `json:"SF_MSG,omitempty"`
	CAMsg *CMsg  `json:"CA_MSG,omitempty"`
	CBMsg *CMsg  `json:"CB_MSG,omitempty"`
	CCMsg *CMsg  `json:"CC_MSG,omitempty"`
}

// GetCMsg is a function to return the CMsg (either a CA, CB or CC) from a Message struct
func GetCMsg(msg *Message) *CMsg {
	if msg.CAMsg != nil {
		return msg.CAMsg
	} else if msg.CBMsg != nil {
		return msg.CBMsg
	} else if msg.CCMsg != nil {
		return msg.CCMsg
	} else {
		return nil // None of the fields is set
	}
}

// UKTrainSchedule is a struct to hold the data from the UK Train Schedules API
type UKTrainSchedule struct {
	ID                     int    `json:"ID"`
	ScheduleID             int    `json:"ScheduleID"`
	CIFBankHolidayRunning  string `json:"CIF_bank_holiday_running"`
	CIFStpIndicator        string `json:"CIF_stp_indicator"`
	CIFTrainUID            string `json:"CIF_train_uid"`
	ApplicableTimetable    string `json:"applicable_timetable"`
	AtocCode               string `json:"atoc_code"`
	AtocCodeDescription    string `json:"atoc_code_description"`
	ScheduleDaysRuns       string `json:"schedule_days_runs"`
	ScheduleEndDate        string `json:"schedule_end_date"`
	ScheduleStartDate      string `json:"schedule_start_date"`
	TrainStatus            string `json:"train_status"`
	TrainStatusDescription string `json:"train_status_description"`
	TransactionType        string `json:"transaction_type"`
	ScheduleSegment        struct {
		ID               int `json:"ID"`
		JSONScheduleV1ID int `json:"JSONScheduleV1ID"`
	} `json:"schedule_segment"`
	NewScheduleSegment struct {
		ID               int `json:"ID"`
		JSONScheduleV1ID int `json:"JSONScheduleV1ID"`
	} `json:"new_schedule_segment"`
	SignallingID                           string `json:"signalling_id"`
	CIFTrainCategory                       string `json:"CIF_train_category"`
	CIFTrainCategoryDescription            string `json:"CIF_train_category_description"`
	CIFCourseIndicator                     int    `json:"CIF_course_indicator"`
	CIFTrainServiceCode                    string `json:"CIF_train_service_code"`
	CIFBusinessSector                      string `json:"CIF_business_sector"`
	CIFPowerType                           string `json:"CIF_power_type"`
	CIFPowerTypeDescription                string `json:"CIF_power_type_description"`
	CIFSpeed                               string `json:"CIF_speed"`
	CIFOperatingCharacteristics            string `json:"CIF_operating_characteristics,omitempty"`
	CIFOperatingCharacteristicsDescription string `json:"CIF_operating_characteristics_description,omitempty"`
	CIFTrainClass                          string `json:"CIF_train_class"`
	ScheduleLocations                      []struct {
		ID                int    `json:"ID"`
		JSONScheduleV1ID  int    `json:"JSONScheduleV1ID"`
		ScheduleSegmentID int    `json:"ScheduleSegmentID"`
		LocationType      string `json:"location_type"`
		RecordIdentity    string `json:"record_identity"`
		TiplocCode        string `json:"tiploc_code"`
		Departure         string `json:"departure,omitempty"`
		PublicDeparture   string `json:"public_departure,omitempty"`
		Platform          string `json:"platform,omitempty"`
		Line              string `json:"line,omitempty"`
		Pass              string `json:"pass,omitempty"`
		Arrival           string `json:"arrival,omitempty"`
		PublicArrival     string `json:"public_arrival,omitempty"`
		PathingAllowance  string `json:"pathing_allowance,omitempty"`
		Path              string `json:"path,omitempty"`
	} `json:"schedule_location"`
	ScheduleEndDateTs            int    `json:"schedule_end_date_ts"`
	ScheduleFromDateTs           int    `json:"schedule_from_date_ts"`
	CIFTimingLoad                string `json:"CIF_timing_load,omitempty"`
	CIFTimingLoadDescription     string `json:"CIF_timing_load_description,omitempty"`
	CIFHeadcode                  string `json:"CIF_headcode,omitempty"`
	CIFReservations              string `json:"CIF_reservations,omitempty"`
	Origin                       string `json:"origin,omitempty"`
	Destination                  string `json:"destination,omitempty"`
	TimeOfDepartureFromOriginTS  int64  `json:"time_of_departure_from_origin_ts"`
	TimeOfArrivalAtDestinationTS int64  `json:"time_of_arrival_at_destination_ts"`
}

// recordTrains is a function to record the video frames for the current passing trains
func recordTrains() {

	maxFrames := 18000

	frame := []byte{}
	for {
		for i, train := range currentPassingTrains {
			if train.HasPassed == true {
				//remove the train from the list
				currentPassingTrains = append(currentPassingTrains[:i], currentPassingTrains[i+1:]...)
			}
		}

		if len(currentPassingTrains) == 0 {
			//no trains to record
			continue
		}

		if videoCaptureDevice != nil {

			frame = <-videoCaptureDevice.GetOutput()

			if len(frame) == 0 {
				log.Println("received frame size 0")
				//sleep for 1 second
				time.Sleep(1 * time.Second)
				continue
			}

			for i, train := range currentPassingTrains {

				currentPassingTrains[i].FrameCount++

				if currentPassingTrains[i].FrameCount > maxFrames {
					trainHasPassed(train.Headcode)
				}

				fileName := fmt.Sprintf("/tmp/%s_%s_%04d.jpg", train.Headcode, train.DateTime, currentPassingTrains[i].FrameCount)
				file, err := os.Create(fileName)
				if err != nil {
					log.Printf("failed to create file %s: %s", fileName, err)
					continue
				}

				if _, err := file.Write(frame); err != nil {
					log.Printf("failed to write file %s: %s", fileName, err)
					file.Close()
					continue
				}

				if err := file.Close(); err != nil {
					log.Printf("failed to close file %s: %s", fileName, err)
					continue
				}
			}
		}
	}
}

// createVideoFromImages is a function to create a video from a list of images
// It uses the mjpeg library to create an mpeg video from a list of JPEG images
func createVideoFromImages(imageFiles []string, outputFilename string) error {

	aw, err := mjpeg.New(outputFilename, 1024, 768, 24)

	if err != nil {
		return err
	}

	// Loop through each JPEG image file
	for _, filename := range imageFiles {
		// Read the JPEG image file
		imageData, err := ioutil.ReadFile(filename)
		if err != nil {
			panic(err)
		}

		err = aw.AddFrame(imageData)
		if err != nil {
			return err
		}
	}

	err = aw.Close()
	if err != nil {
		return err
	}

	fmt.Println("MJPEG video generated successfully!")

	return nil
}

// trainHasArrived is called when a train arrives at one of the berths we are interested in
// It creates a new PassingTrain object and adds it to the list of current passing trains
// It also starts a goroutine to get the train info from the UK Rail API
func trainHasArrived(headcode string) {
	//create a new train recording object and add it to the list of current train recordings
	fmt.Println("Starting recording for train ", headcode)
	var train PassingTrain
	train.Headcode = headcode
	train.DateTime = time.Now().Format("20060102_1504")
	train.HasPassed = false
	currentPassingTrains = append(currentPassingTrains, train)
	go getPassingTrainInfo(&currentPassingTrains[len(currentPassingTrains)-1])
}

// trainHasPassed is called when a train has passed one of the berths we are interested in
// It stops the recording of the train and creates a video from the images
// It also creates a JSON file containing the train info
// It also sends the train info to MQTT if configured
func trainHasPassed(headcode string) {

	fmt.Println("Stopping recording for train ", headcode)
	//find the train recording object in the list of current train recordings and remove it
	for i, train := range currentPassingTrains {
		if train.Headcode == headcode {

			//set the train as passed
			currentPassingTrains[i].HasPassed = true

			train_json, err := json.Marshal(train)
			if err != nil {
				fmt.Println("Error marshalling JSON:", err)
				return
			}

			if mqttClient != nil {
				fmt.Println("Sending train info to MQTT...")
				token := mqttClient.Publish(getConfigValue("mqtt_topic"), 0, false, train_json)
				token.Wait()
				if token.Error() != nil {
					fmt.Println("Error publishing MQTT message:", token.Error())
					return
				}
			}

			fmt.Println("Done capturing video frames. Video conversion starting...")

			//create a list of the image files
			imageFiles := []string{}
			for i := 1; i <= train.FrameCount; i++ {
				imageFiles = append(imageFiles, fmt.Sprintf("/tmp/%s_%s_%04d.jpg", train.Headcode, train.DateTime, i))
			}

			//create the video from the images
			err = createVideoFromImages(imageFiles, fmt.Sprintf("/tmp/%s_%s.mp4", train.Headcode, train.DateTime))
			if err != nil {
				fmt.Println("Error creating video from images:", err)
				return
			} else {
				//delete the image files
				for _, imageFile := range imageFiles {
					err := os.Remove(imageFile)
					if err != nil {
						fmt.Println("Error deleting image file:", err)
					}
				}
			}

			//save the train info to a file
			fmt.Println("Saving train info to file...")
			file, err := os.Create(fmt.Sprintf("/tmp/%s_%s.json", train.Headcode, train.DateTime))
			if err != nil {
				fmt.Println("Error creating file:", err)
				return
			}
			defer file.Close()
			//write the train info to the file
			file.Write(train_json)
			file.Close()

		}

	}
}

// getPassingTrainInfo is a function to get the train info from the UK Rail API
// It uses the UK Rail API to get the train info for the headcode and tiploc code if configured
func getPassingTrainInfo(passingTrain *PassingTrain) error {

	fmt.Println("Getting train info for ", passingTrain.Headcode)

	passingTrain.Origin = "Unknown"
	passingTrain.Destination = "Unknown"
	passingTrain.Operator = "Unknown"
	passingTrain.Category = "Unknown"
	passingTrain.PowerType = "Unknown"
	passingTrain.TimingLoad = "Unknown"
	passingTrain.OperatingCharacteristics = "Unknown"
	passingTrain.Speed = "Unknown"

	apiUrl := "http://lenny:3333/schedules/headcode/" + passingTrain.Headcode

	if getConfigValue("tiploc_code") != "" {
		apiUrl += "?location=" + getConfigValue("tiploc_code")
	}

	// Send an HTTP GET request
	response, err := http.Get(apiUrl)
	if err != nil {
		fmt.Println("Error:", err)
		return err
	}
	defer response.Body.Close()

	// Check if the response status code is OK (200)
	if response.StatusCode != http.StatusOK {
		fmt.Println("HTTP request failed with status code:", response.StatusCode)
		return err
	} else {

		// Create a variable of type UKTrainSchedules to unmarshal the JSON response
		var schedules []UKTrainSchedule

		// Decode the JSON response into the ASTRUCT variable
		decoder := json.NewDecoder(response.Body)
		if err := decoder.Decode(&schedules); err != nil {
			fmt.Println("Error decoding JSON:", err)
			return err
		}

		now := time.Now().Unix()

		var best_match UKTrainSchedule

		for _, schedule := range schedules {
			if schedule.TimeOfArrivalAtDestinationTS > now {
				if best_match.Origin == "" || (schedule.TimeOfArrivalAtDestinationTS < best_match.TimeOfArrivalAtDestinationTS && best_match.Origin != "") {
					best_match = schedule
				}
			}
		}

		if best_match.Origin != "" {
			fmt.Println("Train found: From "+best_match.Origin+" To "+best_match.Destination+" Operated by "+best_match.AtocCodeDescription+" Category ", best_match.CIFTrainCategoryDescription, ", Power Type ", best_match.CIFPowerTypeDescription, ", Timing Load ", best_match.CIFTimingLoadDescription, ", Operating Characteristics ", best_match.CIFOperatingCharacteristicsDescription, ", Speed ", best_match.CIFSpeed, "mph")
			passingTrain.Origin = best_match.Origin
			passingTrain.Destination = best_match.Destination
			passingTrain.Operator = best_match.AtocCodeDescription
			passingTrain.Category = best_match.CIFTrainCategoryDescription
			passingTrain.PowerType = best_match.CIFPowerTypeDescription
			passingTrain.TimingLoad = best_match.CIFTimingLoadDescription
			passingTrain.OperatingCharacteristics = best_match.CIFOperatingCharacteristicsDescription
			passingTrain.Speed = best_match.CIFSpeed
		} else {
			fmt.Println("No suitable train found")
		}
	}

	return nil

}

// This function will listen for trains and starts recording image frames when a train arrives
// and stop recording when it passes
func listenForTrains() error {
	fmt.Println("Starting stomp connection...")

	conn, err := stomp.Dial("tcp", getConfigValue("stomp_url"),
		stomp.ConnOpt.HeartBeatError(360*time.Second),
		stomp.ConnOpt.Login(getConfigValue("stomp_login"), getConfigValue("stomp_password")))

	if err != nil {
		return err
	}
	defer conn.Disconnect()

	sub, err := conn.Subscribe("TD_ALL_SIG_AREA", stomp.AckAuto)
	if err != nil {
		return err
	}

	for {
		stompMsg := <-sub.C
		if stompMsg.Err != nil {
			fmt.Printf("There was an error: %+v", stompMsg.Err)
			return stompMsg.Err
		}

		var messages []Message
		if err := json.Unmarshal(stompMsg.Body, &messages); err != nil {
			fmt.Println("Error decoding JSON:", err)
			return err
		}

		for _, message := range messages {
			if message.CAMsg != nil || message.CBMsg != nil || message.CCMsg != nil {
				c_msg := GetCMsg(&message)
				if c_msg.AreaID == getConfigValue("area_code") {
					//check if the message is for one of the berths we are interested in
					for _, berth := range berths {
						if c_msg.To == berth {
							fmt.Println("START recording ", c_msg.Descr)
							go trainHasArrived(c_msg.Descr)
						} else if c_msg.From == berth {
							fmt.Println("STOP recording ", c_msg.Descr)
							go trainHasPassed(c_msg.Descr)
						}
					}
				}

				/*
				if c_msg.AreaID == getConfigValue("area_code") && (c_msg.To == "2207" || c_msg.To == "2196") {
				fmt.Println("START recording ", c_msg.Descr)
				go trainHasArrived(c_msg.Descr)
				}
				if c_msg.AreaID == "B1" && (c_msg.From == "2207" || c_msg.From == "2196") {
				fmt.Println("STOP recording ", c_msg.Descr)
				go trainHasPassed(c_msg.Descr)
				}
				*/
			}
		}

	}

	return nil

}
