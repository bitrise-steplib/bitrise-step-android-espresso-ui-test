package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bitrise-io/go-utils/colorstring"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/sliceutil"
	"github.com/bitrise-io/go-utils/urlutil"
	"github.com/bitrise-tools/go-android/gradle"
	"github.com/bitrise-tools/go-steputils/input"
	"github.com/bitrise-tools/go-steputils/stepconf"
	"github.com/bitrise-tools/go-steputils/tools"
	testing "google.golang.org/api/testing/v1"
	toolresults "google.golang.org/api/toolresults/v1beta3"
)

// Config ...
type Config struct {
	// gradle
	ProjectLocation string `env:"project_location,dir"`
	Variant         string `env:"variant"`
	Module          string `env:"module,required"`

	// api
	APIBaseURL string `env:"api_base_url,required"`
	BuildSlug  string `env:"BITRISE_BUILD_SLUG,required"`
	AppSlug    string `env:"BITRISE_APP_SLUG,required"`
	APIToken   string `env:"api_token,required"`

	// shared
	TestDevices          string `env:"test_devices,required"`
	AppPackageID         string `env:"app_package_id"`
	TestTimeout          string `env:"test_timeout"`
	DownloadTestResults  string `env:"download_test_results"`
	DirectoriesToPull    string `env:"directories_to_pull"`
	EnvironmentVariables string `env:"environment_variables"`

	// instrumentation
	TestPackageID   string `env:"test_package_id"`
	TestRunnerClass string `env:"test_runner_class"`
	TestTargets     string `env:"test_targets"`
	UseOrchestrator string `env:"use_orchestrator"`
}

// UploadURLRequest ...
type UploadURLRequest struct {
	AppURL     string `json:"appUrl"`
	TestAppURL string `json:"testAppUrl"`
}

func (c Config) print() {
	log.Infof("Configs:")
	log.Printf("- ProjectLocation: %s", c.ProjectLocation)
	log.Printf("- Module: %s", c.Module)
	log.Printf("- Variant: %s", c.Variant)
	log.Printf("- TestTimeout: %s", c.TestTimeout)
	log.Printf("- DirectoriesToPull: %s", c.DirectoriesToPull)
	log.Printf("- EnvironmentVariables: %s", c.EnvironmentVariables)
	log.Printf("- TestDevices:\n---")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(w, "Model\tAPI Level\tLocale\tOrientation\t"); err != nil {
		failf("Failed to write to the writer, error: %s", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(c.TestDevices))
	for scanner.Scan() {
		device := scanner.Text()
		device = strings.TrimSpace(device)
		if device == "" {
			continue
		}

		deviceParams := strings.Split(device, ",")

		if len(deviceParams) != 4 {
			continue
		}

		if _, err := fmt.Fprintln(w, fmt.Sprintf("%s\t%s\t%s\t%s\t", deviceParams[0], deviceParams[1], deviceParams[2], deviceParams[3])); err != nil {
			failf("Failed to write to the writer, error: %s", err)
		}
	}
	if err := w.Flush(); err != nil {
		log.Errorf("Failed to flush writer, error: %s", err)
	}
	log.Printf("---")

	log.Printf("- AppPackageID: %s", c.AppPackageID)
	log.Printf("- TestPackageID: %s", c.TestPackageID)
	log.Printf("- TestRunnerClass: %s", c.TestRunnerClass)
	log.Printf("- TestTargets: %s", c.TestTargets)
	log.Printf("- UseOrchestrator: %s", c.UseOrchestrator)
}

func (c Config) validate() error {
	if err := input.ValidateIfNotEmpty(c.APIBaseURL); err != nil {
		if _, set := os.LookupEnv("BITRISE_IO"); set {
			log.Warnf("Warning: please make sure that Virtual Device Testing add-on is turned on under your app's settings tab.")
		}
		return fmt.Errorf("Issue with APIBaseURL: %s", err)
	}
	if err := input.ValidateIfNotEmpty(c.APIToken); err != nil {
		return fmt.Errorf("Issue with APIToken: %s", err)
	}
	if err := input.ValidateIfNotEmpty(c.BuildSlug); err != nil {
		return fmt.Errorf("Issue with BuildSlug: %s", err)
	}
	if err := input.ValidateIfNotEmpty(c.AppSlug); err != nil {
		return fmt.Errorf("Issue with AppSlug: %s", err)
	}
	if err := input.ValidateWithOptions(c.UseOrchestrator, "false", "true"); err != nil {
		return fmt.Errorf("Issue with UseOrchestrator: %s", err)
	}
	return nil
}

func failf(f string, v ...interface{}) {
	log.Errorf(f, v...)
	os.Exit(1)
}

func uploadAPKs(apiBaseURL, appSlug, buildSlug, apiToken, appAPKPath, testAPKPath string) error {
	url, err := urlutil.Join(apiBaseURL, "assets", appSlug, buildSlug, apiToken)
	if err != nil {
		return fmt.Errorf("failed to join url, error: %s", err)
	}

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create http request, error: %s", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get http response, error: %s", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body, error: %s", err)
		}
		return fmt.Errorf("failed to start test: %d, error: %s", resp.StatusCode, string(body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body, error: %s", err)
	}

	responseModel := &UploadURLRequest{}

	if err = json.Unmarshal(body, responseModel); err != nil {
		return fmt.Errorf("failed to unmarshal response body, error: %s", err)
	}

	if err = uploadFile(responseModel.AppURL, appAPKPath); err != nil {
		return fmt.Errorf("Failed to upload file(%s) to (%s), error: %s", appAPKPath, responseModel.AppURL, err)
	}

	if err = uploadFile(responseModel.TestAppURL, testAPKPath); err != nil {
		return fmt.Errorf("Failed to upload file(%s) to (%s), error: %s", testAPKPath, responseModel.TestAppURL, err)
	}
	return nil
}

func startTest(c Config) error {
	url, err := urlutil.Join(c.APIBaseURL, c.AppSlug, c.BuildSlug, c.APIToken)
	if err != nil {
		return fmt.Errorf("failed to join url, error: %s", err)
	}

	testModel := &testing.TestMatrix{}
	testModel.EnvironmentMatrix = &testing.EnvironmentMatrix{AndroidDeviceList: &testing.AndroidDeviceList{}}
	testModel.EnvironmentMatrix.AndroidDeviceList.AndroidDevices = []*testing.AndroidDevice{}

	scanner := bufio.NewScanner(strings.NewReader(c.TestDevices))
	for scanner.Scan() {
		device := scanner.Text()
		device = strings.TrimSpace(device)
		if device == "" {
			continue
		}

		deviceParams := strings.Split(device, ",")
		if len(deviceParams) != 4 {
			return fmt.Errorf("invalid test device configuration: %s", device)
		}

		newDevice := testing.AndroidDevice{
			AndroidModelId:   deviceParams[0],
			AndroidVersionId: deviceParams[1],
			Locale:           deviceParams[2],
			Orientation:      deviceParams[3],
		}

		testModel.EnvironmentMatrix.AndroidDeviceList.AndroidDevices = append(testModel.EnvironmentMatrix.AndroidDeviceList.AndroidDevices, &newDevice)
	}

	// parse directories to pull
	scanner = bufio.NewScanner(strings.NewReader(c.DirectoriesToPull))
	directoriesToPull := []string{}
	for scanner.Scan() {
		path := scanner.Text()
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		directoriesToPull = append(directoriesToPull, path)
	}

	// parse environment variables
	scanner = bufio.NewScanner(strings.NewReader(c.EnvironmentVariables))
	envs := []*testing.EnvironmentVariable{}
	for scanner.Scan() {
		envStr := scanner.Text()

		if envStr == "" {
			continue
		}

		if !strings.Contains(envStr, "=") {
			continue
		}

		envStrSplit := strings.Split(envStr, "=")
		envKey := envStrSplit[0]
		envValue := strings.Join(envStrSplit[1:], "=")

		envs = append(envs, &testing.EnvironmentVariable{Key: envKey, Value: envValue})
	}

	testModel.TestSpecification = &testing.TestSpecification{
		TestTimeout: fmt.Sprintf("%ss", c.TestTimeout),
		TestSetup: &testing.TestSetup{
			EnvironmentVariables: envs,
			DirectoriesToPull:    directoriesToPull,
		},
	}

	testModel.TestSpecification.AndroidInstrumentationTest = &testing.AndroidInstrumentationTest{}
	if c.AppPackageID != "" {
		testModel.TestSpecification.AndroidInstrumentationTest.AppPackageId = c.AppPackageID
	}
	if c.TestPackageID != "" {
		testModel.TestSpecification.AndroidInstrumentationTest.TestPackageId = c.TestPackageID
	}
	if c.TestRunnerClass != "" {
		testModel.TestSpecification.AndroidInstrumentationTest.TestRunnerClass = c.TestRunnerClass
	}
	if c.TestTargets != "" {
		targets := strings.Split(strings.TrimSpace(c.TestTargets), ",")
		testModel.TestSpecification.AndroidInstrumentationTest.TestTargets = targets
	}
	if c.UseOrchestrator == "true" {
		testModel.TestSpecification.AndroidInstrumentationTest.OrchestratorOption = "USE_ORCHESTRATOR"
	} else {
		testModel.TestSpecification.AndroidInstrumentationTest.OrchestratorOption = "DO_NOT_USE_ORCHESTRATOR"
	}

	jsonByte, err := json.Marshal(testModel)
	if err != nil {
		return fmt.Errorf("failed to marshal test model, error: %s", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonByte))
	if err != nil {
		return fmt.Errorf("failed to create http request, error: %s", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get http response, error: %s", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body, error: %s", err)
		}
		return fmt.Errorf("failed to start test: %d, error: %s", resp.StatusCode, string(body))
	}

	return nil
}

func waitForResults(apiBaseURL, appSlug, buildSlug, apiToken string) error {
	successful := true
	finished := false
	printedLogs := []string{}
	for !finished {
		url, err := urlutil.Join(apiBaseURL, appSlug, buildSlug, apiToken)
		if err != nil {
			return fmt.Errorf("failed to join url, error: %s", err)
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			failf("Failed to create http request, error: %s", err)
		}

		client := &http.Client{}
		resp, err := client.Do(req)
		if resp.StatusCode != http.StatusOK || err != nil {
			resp, err = client.Do(req)
			if err != nil {
				failf("Failed to get http response, error: %s", err)
			}
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			failf("Failed to read response body, error: %s", err)
		}

		if resp.StatusCode != http.StatusOK {
			failf("Failed to get test status, error: %s", string(body))
		}

		responseModel := &toolresults.ListStepsResponse{}

		if err = json.Unmarshal(body, responseModel); err != nil {
			failf("Failed to unmarshal response body, error: %s, body: %s", err, string(body))
		}

		finished = true
		testsRunning := 0
		for _, step := range responseModel.Steps {
			if step.State != "complete" {
				finished = false
				testsRunning++
			}
		}

		msg := ""
		if len(responseModel.Steps) == 0 {
			finished = false
			msg = fmt.Sprintf("- Validating")
		} else {
			msg = fmt.Sprintf("- (%d/%d) running", testsRunning, len(responseModel.Steps))
		}

		if !sliceutil.IsStringInSlice(msg, printedLogs) {
			log.Printf(msg)
			printedLogs = append(printedLogs, msg)
		}

		if !finished {
			time.Sleep(5 * time.Second)
			continue
		}

		log.Donef("=> Test finished")
		fmt.Println()

		log.Infof("Test results:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		if _, err := fmt.Fprintln(w, "Model\tAPI Level\tLocale\tOrientation\tOutcome\t"); err != nil {
			failf("Failed to write to the writer, error: %s", err)
		}

		for _, step := range responseModel.Steps {
			dimensions := map[string]string{}
			for _, dimension := range step.DimensionValue {
				dimensions[dimension.Key] = dimension.Value
			}

			outcome := step.Outcome.Summary

			switch outcome {
			case "success":
				outcome = colorstring.Green(outcome)
			case "failure":
				successful = false
				if step.Outcome.FailureDetail != nil {
					if step.Outcome.FailureDetail.Crashed {
						outcome += "(Crashed)"
					}
					if step.Outcome.FailureDetail.NotInstalled {
						outcome += "(NotInstalled)"
					}
					if step.Outcome.FailureDetail.OtherNativeCrash {
						outcome += "(OtherNativeCrash)"
					}
					if step.Outcome.FailureDetail.TimedOut {
						outcome += "(TimedOut)"
					}
					if step.Outcome.FailureDetail.UnableToCrawl {
						outcome += "(UnableToCrawl)"
					}
				}
				outcome = colorstring.Red(outcome)
			case "inconclusive":
				successful = false
				if step.Outcome.InconclusiveDetail != nil {
					if step.Outcome.InconclusiveDetail.AbortedByUser {
						outcome += "(AbortedByUser)"
					}
					if step.Outcome.InconclusiveDetail.InfrastructureFailure {
						outcome += "(InfrastructureFailure)"
					}
				}
				outcome = colorstring.Yellow(outcome)
			case "skipped":
				successful = false
				if step.Outcome.SkippedDetail != nil {
					if step.Outcome.SkippedDetail.IncompatibleAppVersion {
						outcome += "(IncompatibleAppVersion)"
					}
					if step.Outcome.SkippedDetail.IncompatibleArchitecture {
						outcome += "(IncompatibleArchitecture)"
					}
					if step.Outcome.SkippedDetail.IncompatibleDevice {
						outcome += "(IncompatibleDevice)"
					}
				}
				outcome = colorstring.Blue(outcome)
			}

			if _, err := fmt.Fprintln(w, fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t", dimensions["Model"], dimensions["Version"], dimensions["Locale"], dimensions["Orientation"], outcome)); err != nil {
				failf("Failed to write to the writer, error: %s", err)
			}
		}
		if err := w.Flush(); err != nil {
			log.Errorf("Failed to flush writer, error: %s", err)
		}
	}
	if !successful {
		return fmt.Errorf("one or more test failed")
	}
	return nil
}

func downloadAssets(apiBaseURL, appSlug, buildSlug, apiToken string) error {
	url, err := urlutil.Join(apiBaseURL, "assets", appSlug, buildSlug, apiToken)
	if err != nil {
		return fmt.Errorf("failed to join url, error: %s", err)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("Failed to create http request, error: %s", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to get http response, error: %s", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Failed to get http response, status code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Failed to read response body, error: %s", err)
	}

	responseModel := map[string]string{}

	if err = json.Unmarshal(body, &responseModel); err != nil {
		return fmt.Errorf("Failed to unmarshal response body, error: %s", err)
	}

	tempDir, err := pathutil.NormalizedOSTempDirPath("vdtesting_test_assets")
	if err != nil {
		return fmt.Errorf("Failed to create temp dir, error: %s", err)
	}

	for fileName, fileURL := range responseModel {
		if err := downloadFile(fileURL, filepath.Join(tempDir, fileName)); err != nil {
			return fmt.Errorf("Failed to download file, error: %s", err)
		}
	}

	log.Donef("=> Assets downloaded")
	if err := tools.ExportEnvironmentWithEnvman("VDTESTING_DOWNLOADED_FILES_DIR", tempDir); err != nil {
		log.Warnf("Failed to export environment (VDTESTING_DOWNLOADED_FILES_DIR), error: %s", err)
	} else {
		log.Printf("The downloaded test assets path (%s) is exported to the VDTESTING_DOWNLOADED_FILES_DIR environment variable.", tempDir)
	}
	return nil
}

func downloadFile(url string, localPath string) error {
	out, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("Failed to open the local cache file for write: %s", err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			log.Printf("Failed to close Archive download file (%s): %s", localPath, err)
		}
	}()

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("Failed to create cache download request: %s", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("Failed to close Archive download response body: %s", err)
		}
	}()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Failed to download archive - non success response code: %d", resp.StatusCode)
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("Failed to save cache content into file: %s", err)
	}

	return nil
}

func uploadFile(uploadURL string, archiveFilePath string) error {
	archFile, err := os.Open(archiveFilePath)
	if err != nil {
		return fmt.Errorf("Failed to open archive file for upload (%s): %s", archiveFilePath, err)
	}
	isFileCloseRequired := true
	defer func() {
		if !isFileCloseRequired {
			return
		}
		if err := archFile.Close(); err != nil {
			log.Printf(" (!) Failed to close archive file (%s): %s", archiveFilePath, err)
		}
	}()

	fileInfo, err := archFile.Stat()
	if err != nil {
		return fmt.Errorf("Failed to get File Stats of the Archive file (%s): %s", archiveFilePath, err)
	}
	fileSize := fileInfo.Size()

	req, err := http.NewRequest("PUT", uploadURL, archFile)
	if err != nil {
		return fmt.Errorf("Failed to create upload request: %s", err)
	}

	req.Header.Add("Content-Length", strconv.FormatInt(fileSize, 10))
	req.ContentLength = fileSize

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to upload: %s", err)
	}
	isFileCloseRequired = false
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf(" [!] Failed to close response body: %s", err)
		}
	}()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Failed to read response: %s", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("Failed to upload file, response code was: %d, body: %s", resp.StatusCode, string(b))
	}

	return nil
}

func main() {
	var config Config

	if err := stepconf.Parse(&config); err != nil {
		failf("Couldn't create config: %v\n", err)
	}

	if err := config.validate(); err != nil {
		failf("%s", err)
	}

	config.print()
	fmt.Println()

	gradleProject, err := gradle.NewProject(config.ProjectLocation)
	if err != nil {
		failf("Failed to open project, error: %s", err)
	}

	buildTask := gradleProject.GetTask("assemble")

	log.Infof("Testable variants:")

	variantsMap, err := buildTask.GetVariants()
	if err != nil {
		failf("Failed to fetch variants, error: %s", err)
	}

	variants, ok := variantsMap[config.Module]
	if !ok {
		failf("No module with name: %s found", config.Module)
	}

	// get DebugAndroidTest ended tasks
	testVariants := []string{}
	for _, variant := range variants {
		if strings.HasSuffix(variant, "DebugAndroidTest") {
			testVariants = append(testVariants, variant)
		}
	}

	// check non test tasks that has the same prefix as the test tasks and collect them as pairs
	type match struct {
		variant  string
		appTask  string
		testTask string
	}
	var matches []match
	for _, variant := range variants {
		for _, testVariant := range testVariants {
			v := strings.TrimSuffix(testVariant, "DebugAndroidTest")
			if variant == v+"Debug" {
				matches = append(matches, match{v, variant, testVariant})
			}
		}
	}

	var selectedMatch match
	if len(matches) == 1 && matches[0].variant == "" {
		selectedMatch = matches[0]
		log.Warnf("Your project configuration has no variants, ignoring Variant input...")
	} else {
		for _, match := range matches {
			if config.Variant == match.variant {
				log.Donef("✓ %s", match.variant)
				selectedMatch = match
			} else {
				log.Printf("- %s", match.variant)
			}
		}
	}

	if selectedMatch.appTask == "" {
		failf("The given variant: \"%s\" does not match any buildable variant from the list above.", config.Variant)
	}

	fmt.Println()

	started := time.Now()

	log.Infof("Build app APK:")
	if err := buildTask.GetCommand(gradle.Variants{config.Module: []string{selectedMatch.appTask}}).Run(); err != nil {
		failf("Build task failed, error: %v", err)
	}
	fmt.Println()

	log.Infof("Find generated app APK:")

	appDebugAPKPattern := "*.apk"

	appAPKs, err := gradleProject.FindArtifacts(started, appDebugAPKPattern, false)
	if err != nil {
		failf("failed to find apks, error: %v", err)
	}

	if len(appAPKs) == 0 {
		failf("No APKs found with pattern: %s", appDebugAPKPattern)
	}

	if len(appAPKs) > 1 {
		failf("Multiple APKs found, only one supported. (%v)", appAPKs)
	}

	log.Printf("- %s", appAPKs[0].Name)

	fmt.Println()

	started = time.Now()

	log.Infof("Build test APK:")
	if err = buildTask.GetCommand(gradle.Variants{config.Module: []string{selectedMatch.testTask}}).Run(); err != nil {
		failf("Build task failed, error: %v", err)
	}

	fmt.Println()

	log.Infof("Find generated test APK:")

	testDebugAPKPattern := "*.apk"

	testAPKs, err := gradleProject.FindArtifacts(started, testDebugAPKPattern, false)
	if err != nil {
		failf("failed to find apks, error: %v", err)
	}

	if len(testAPKs) == 0 {
		failf("No test apks found with pattern: %s", testDebugAPKPattern)
	}

	if len(testAPKs) > 1 {
		failf("Multiple test APKs found, only one supported. (%v)", testAPKs)
	}

	log.Printf("- %s", testAPKs[0].Name)
	fmt.Println()

	log.Infof("Upload APKs")
	log.Printf("- %s", appAPKs[0].Path)
	log.Printf("- %s", testAPKs[0].Path)

	if err := uploadAPKs(config.APIBaseURL, config.AppSlug, config.BuildSlug, config.APIToken, appAPKs[0].Path, testAPKs[0].Path); err != nil {
		failf("Failed to upload APKs, error: %s", err)
	}
	log.Donef("=> APKs uploaded")

	fmt.Println()

	log.Infof("Start test")
	if err := startTest(config); err != nil {
		failf("Failed to start test, error: %s", err)
	}
	log.Donef("=> Test started")

	fmt.Println()
	log.Infof("Waiting for test results")
	if err := waitForResults(config.APIBaseURL, config.AppSlug, config.BuildSlug, config.APIToken); err != nil {
		failf("An issue encountered getting the test results, error: %s", err)
	}

	if config.DownloadTestResults == "true" {
		fmt.Println()
		log.Infof("Downloading test assets")
		if err := downloadAssets(config.APIBaseURL, config.AppSlug, config.BuildSlug, config.APIToken); err != nil {
			failf("failed to download test assets, error: %s", err)
		}
	}
}
