package singul

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"path/filepath"

	uuid "github.com/satori/go.uuid"

	"github.com/frikky/schemaless"
	"github.com/shuffle/shuffle-shared"
	"github.com/frikky/kin-openapi/openapi3"
)

var standalone = false
var debug = os.Getenv("DEBUG") == "true"
var basepath = os.Getenv("SHUFFLE_FILE_LOCATION")

// Set up 3rd party connections IF necessary
var shuffleApiKey = os.Getenv("SHUFFLE_AUTHORIZATION")
var shuffleBackend = os.Getenv("SHUFFLE_BACKEND")
var shuffleOrg = os.Getenv("SHUFFLE_ORG")

// Apps to test:
// Communication: Slack & Teams -> Send message
// Ticketing: TheHive & Jira 		-> Create ticket

// 1. Check the category, if it has the label in it
// 2. Get the app (openapi & app config)
// 3. Check if the label exists for the app in any action
// 4. Do the translation~ (workflow or direct)
// 5. Run app action/workflow
// 6. Return result from the app
func RunCategoryAction(resp http.ResponseWriter, request *http.Request) {
	cors := shuffle.HandleCors(resp, request)
	if cors {
		return
	}

	// Just here to verify that the user is logged in
	if resp != nil {
		resp.Header().Add("x-execution-url", "")
		resp.Header().Add("x-apprun-url", "")
	}

	ctx := shuffle.GetContext(request)
	user, err := shuffle.HandleApiAuthentication(resp, request)
	if err != nil {
		// Look for "authorization" and "execution_id" queries
		authorization := request.URL.Query().Get("authorization")
		executionId := request.URL.Query().Get("execution_id")
		if len(authorization) == 0 || len(executionId) == 0 {
			log.Printf("[AUDIT] Api authentication failed in run category action: %s. Normal auth and Execution Auth not found. %#v & %#v", err, authorization, executionId)

			resp.WriteHeader(401)
			resp.Write([]byte(`{"success": false, "reason": "Authentication failed. Sign up first."}`))
			return
		}

		//log.Printf("[INFO] Running category action request with authorization and execution_id: %s, %s", authorization, executionId)
		// 1. Get the executionId and check if it's valid
		// 2. Check if authorization is valid
		// 3. Check if the execution is FINISHED or not
		exec, err := shuffle.GetWorkflowExecution(ctx, executionId)
		if err != nil {
			log.Printf("[WARNING] Error with getting execution in run category action: %s", err)
			resp.WriteHeader(400)
			resp.Write([]byte(`{"success": false, "reason": "Failed to get execution"}`))
			return
		}

		if !debug {
			if exec.Status != "EXECUTING" {
				log.Printf("[WARNING][%s] Execution is not executing in run category action: %s", exec.ExecutionId, exec.Status)
				resp.WriteHeader(403)
				resp.Write([]byte(`{"success": false, "reason": "Execution is not executing. Can't modify."}`))
				return
			}
		} else {
			log.Printf("[DEBUG] Debug mode is on. Skipping status check in run category action")
		}

		// 4. Check if the user is the owner of the execution
		if exec.Authorization != authorization {
			log.Printf("[WARNING] Authorization doesn't match in run category action: %s != %s", exec.Authorization, authorization)
			resp.WriteHeader(403)
			resp.Write([]byte(`{"success": false, "reason": "Authorization doesn't match"}`))
			return
		}

		user.Role = "admin"
		user.ActiveOrg.Id = exec.ExecutionOrg
	}

	if user.Role == "org-reader" {
		log.Printf("[WARNING] Org-reader doesn't have access to run category action: %s (%s)", user.Username, user.Id)
		resp.WriteHeader(403)
		resp.Write([]byte(`{"success": false, "reason": "Read only user"}`))
		return
	}

	request.Header.Add("Org-Id", user.ActiveOrg.Id)
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		log.Printf("[WARNING] Error with body read for category action: %s", err)
		resp.WriteHeader(400)
		resp.Write([]byte(`{"success": false, "reason": "Failed reading body of the request"}`))
		return
	}

	org, err := shuffle.GetOrg(ctx, user.ActiveOrg.Id)
	if err != nil {
		log.Printf("[ERROR] User %s has no active org", user.Id)
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success": false, "reason": "User has no active org"}`))
		return
	}

	backendRegion := os.Getenv("SHUFFLE_GCEPROJECT_REGION")
	if len(backendRegion) == 0 {
		backendRegion = "europe-west2"
	}
	
	if len(org.Region) == 0 {
		org.Region = "europe-west2"
	}

	if org.Region != backendRegion {
		log.Printf("[ERROR] Incorrect region for AI request. Backend is in %s", org.Region)
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success": false, "reason": "Region was made to the incorrect org"}`))
		return
	}

	var value shuffle.CategoryAction
	err = json.Unmarshal(body, &value)
	if err != nil {
		log.Printf("[WARNING] Error with unmarshal input body in category action: %s", err)

		if strings.Contains(fmt.Sprintf("%s", err), "CategoryAction.fields") {
			var tmpValue shuffle.CategoryActionFieldOverride
			newerr := json.Unmarshal(body, &tmpValue)
			if newerr != nil {
				log.Printf("[WARNING] Error with unmarshal input body in category action (tmpValue): %s", newerr)
			} else {
				for key, val := range tmpValue.Fields {
					if val != nil {
						value.Fields = append(value.Fields, shuffle.Valuereplace{
							Key:   key,
							Value: fmt.Sprintf("%v", val),
						})
					}
				}
			}

			if len(value.Fields) > 0 {
				err = nil
			}
		}

		if err != nil {
			if debug {
				log.Printf("[DEBUG] FAILING INPUT BODY:\n%s", string(body))
			}
			resp.WriteHeader(400)
			resp.Write([]byte(`{"success": false, "reason": "Failed unmarshaling body of the request"}`))
			return
		}
	}

	RunActionWrapper(ctx, user, value, resp, request)
}

type outputMarshal struct {
	Success bool `json:"success"`
	Exception string `json:"exception"`
	Reason  string `json:"reason"`
}

func setupEnv() {
	// Self-ran Singul versions
	standaloneEnv := os.Getenv("STANDALONE")
	if standaloneEnv == "true" {
		standalone = true

		filepath := os.Getenv("FILE_LOCATION")
		if len(filepath) > 0 {
			basepath = filepath
		}

		if debug {
			log.Printf("[DEBUG] Using local storage path '%s' for files", basepath)
		}
	}
}

func RunAction(ctx context.Context, value shuffle.CategoryAction, retries ...int) (string, error) {
	setupEnv() 

	retriesCount := 0
	if len(retries) > 0 {
		retriesCount = retries[0]
	}

	resp := http.ResponseWriter(nil)
	request := &http.Request{}
	user := shuffle.User{}

	foundResponse, err := RunActionWrapper(ctx, user, value, resp, request)
	if strings.Contains(string(foundResponse), `success": false`) {
		outputString := outputMarshal{}
		jsonerr := json.Unmarshal(foundResponse, &outputString)
		if jsonerr != nil {
			log.Printf("[WARNING] Error with unmarshaling output in run action: %s", jsonerr)
			return string(foundResponse), jsonerr
		}

		if retriesCount == 0 && strings.Contains(outputString.Reason, "Authenticate") && strings.Contains(outputString.Reason, "first") {
			appsplit := strings.Split(outputString.Reason, " ")
			if len(appsplit) > 2 {
				err := AuthenticateAppCli(appsplit[1])
				if err != nil {
					return string(foundResponse), err
				} else {
					return RunAction(ctx, value, 1)
				}
			}
		}

		reMarshalled, err := json.MarshalIndent(outputString, "", "  ")
		if err != nil {
			log.Printf("[WARNING] Error with marshaling output in run action: %s", err)
			return string(foundResponse), err
		}

		return string(reMarshalled), err
	}

	return string(foundResponse), err
}

func GetActionAIResponseWrapper(ctx context.Context, input shuffle.QueryInput) ([]byte, error) {
	resp := NewFakeResponseWriter()

	foundResponse, err := shuffle.GetActionAIResponse(ctx, resp, shuffle.User{}, shuffle.Org{}, input.OutputFormat, input)
	if err != nil {
		log.Printf("[WARNING] Error with getting action AI response: %s", err)
	}

	return foundResponse, err
}

func RunActionWrapper(ctx context.Context, user shuffle.User, value shuffle.CategoryAction, resp http.ResponseWriter, request *http.Request) ([]byte, error) {
	// Ensures avoidance of nil-pointers in resp.WriteHeader()
	if resp == nil {
		resp = NewFakeResponseWriter() 
	}

	// Edgecases 
	if value.AppName == "noapp" {
		value.AppName = ""
	}

	if value.App == "noapp" {
		value.AppName = ""
	}

	if len(value.AppName) == 0 && len(value.App) > 0 {
		value.AppName = value.App
	}

	if len(value.Label) == 0 && len(value.Action) > 0 {
		value.Label = value.Action
	}

	respBody := []byte{}
	if len(value.Label) == 0 {
		respBody = []byte(`{"success": false, "reason": "No label set in category action"}`)
		resp.WriteHeader(400)
		resp.Write(respBody)
		return respBody, fmt.Errorf("No label set in category action")
	}

	//log.Printf("[INFO] Running category-action '%s' in category '%s' with app %s for org %s (%s)", value.Label, value.Category, value.AppName, user.ActiveOrg.Name, user.ActiveOrg.Id)

	if len(value.Query) > 0 {
		// Check if app authentication. If so, check if intent is to actually authenticate, or find the actual intent
		if value.Label == "authenticate_app" && !strings.Contains(strings.ToLower(value.Query), "auth") {
			log.Printf("[INFO] Got authenticate app request. Does the intent %#v match auth?", value.Query)
		}
	}

	if value.Label == "discover_app" {
		for _, field := range value.Fields {
			if field.Key == "action" {
				log.Printf("[INFO] NOT IMPLEMENTED: Changing to action label '%s' from discover_app", field.Value)
				//value.Label = field.Value
				break
			}
		}
	}

	if len(value.AppId) > 0 && len(value.AppName) == 0 {
		value.AppName = value.AppId
	}

	if len(value.AppName) == 0 {
		for _, field := range value.Fields {
			lowerkey := strings.ReplaceAll(strings.ToLower(field.Key), " ", "_")
			if lowerkey == "appname" || lowerkey == "app_name" {
				value.AppName = field.Value
				break
			}
		}
	}

	threadId := value.WorkflowId
	if len(value.WorkflowId) > 0 {
		// Should maybe cache this based on the thread? Then reuse and connect?
		if strings.HasPrefix(value.WorkflowId, "thread") {
			threadCache, err := shuffle.GetCache(ctx, value.WorkflowId)
			if err != nil {
				//log.Printf("[WARNING] Error with getting thread cache in category action: %s", err)
			} else {
				// Make threadcache into a string and map it to value.WorkflowId
				// Then use that to get the thread cache
				value.WorkflowId = string([]byte(threadCache.([]uint8)))
			}
		}
	}

	categories := shuffle.GetAllAppCategories()

	value.Category = strings.ToLower(value.Category)
	value.Label = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value.Label)), " ", "_")
	value.AppName = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value.AppName)), " ", "_")

	if value.AppName == "email" {
		value.Category = "email"
		value.AppName = ""
	}

	foundIndex := -1
	labelIndex := -1
	if len(value.Category) > 0 {

		for categoryIndex, category := range categories {
			if strings.ToLower(category.Name) != value.Category {
				continue
			}

			foundIndex = categoryIndex
			break
		}

		if foundIndex >= 0 {
			for _, label := range categories[foundIndex].ActionLabels {
				if strings.ReplaceAll(strings.ToLower(label), " ", "_") == value.Label {
					labelIndex = foundIndex
				}
			}
		}
	} else {
		//log.Printf("[DEBUG] No category set in category action")
	}

	_ = labelIndex

	//log.Printf("[INFO] Found label '%s' in category '%s'. Indexes for category: %d, and label: %d", value.Label, value.Category, foundIndex, labelIndex)

	var err error
	org := &shuffle.Org{}
	newapps := []shuffle.WorkflowApp{}
	if !standalone { 
		newapps, err = shuffle.GetPrioritizedApps(ctx, user)
		if err != nil {
			log.Printf("[WARNING] Failed getting apps in category action: %s", err)
			respBody = []byte(`{"success": false, "reason": "Failed loading apps. Contact support@shuffler.io"}`)
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		org, err = shuffle.GetOrg(ctx, user.ActiveOrg.Id)
		if err != nil {
			log.Printf("[ERROR] Failed getting org %s (%s) in category action: %s", user.ActiveOrg.Name, user.ActiveOrg.Id, err)
		}
	} else {
		//log.Printf("[INFO] Standalone mode. Loading details for app %s", value.AppName)

		if len(value.AppName) == 0 {
			respBody = []byte(`{"success": false, "reason": "Provide an app name in the request"}`)
			resp.WriteHeader(400)
			resp.Write(respBody)
			return respBody, fmt.Errorf("No app name set in category action")
		}

		relevantApp, err := shuffle.GetSingulApp(basepath, value.AppName)
		if err != nil {
			//log.Printf("[WARNING] Failed getting app %s in category action: %s", value.AppName, err)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed getting app %s"}`, value.AppName))
			resp.WriteHeader(400)
			resp.Write(respBody)
			return respBody, err
		}

		newapps = append(newapps, *relevantApp)
	}

	if value.Category == "email" {
		value.Category = "communication"
	}

	// Check the app framework
	foundAppType := value.Category
	foundCategory := shuffle.Category{}
	if foundAppType == "cases" {
		foundCategory = org.SecurityFramework.Cases
	} else if foundAppType == "communication" {
		foundCategory = org.SecurityFramework.Communication
	} else if foundAppType == "assets" {
		foundCategory = org.SecurityFramework.Assets
	} else if foundAppType == "network" {
		foundCategory = org.SecurityFramework.Network
	} else if foundAppType == "intel" {
		foundCategory = org.SecurityFramework.Intel
	} else if foundAppType == "edr" {
		foundCategory = org.SecurityFramework.EDR
	} else if foundAppType == "iam" {
		foundCategory = org.SecurityFramework.IAM
	} else if foundAppType == "siem" {
		foundCategory = org.SecurityFramework.SIEM
	} else if foundAppType == "ai" {
		foundCategory = org.SecurityFramework.AI
	} else {
		if len(foundAppType) > 0 {
			log.Printf("[ERROR] Unknown app type in category action: %#v", foundAppType)
			foundCategory = shuffle.Category{
				Name: foundAppType,
				ID:   foundAppType,
			}
		}
	}

	if foundCategory.Name == "" && value.AppName == "" {
		log.Printf("[DEBUG] No category found AND no app name set in category action for user %s (%s). Returning", user.Username, user.Id)

		parsedText := fmt.Sprintf("Help us set up an app in the '%s' category for you first", foundAppType)
		if len(foundAppType) == 0 {
			parsedText = "Please find the app you would like to use, OR be more explicit about what app you want to use in your request"
		}

		structuredFeedback := shuffle.StructuredCategoryAction{
			Success:  false,
			Action:   "select_category",
			Category: foundAppType,
			Reason:   parsedText,
		}

		jsonBytes, err := json.Marshal(structuredFeedback)
		if err != nil {
			log.Printf("[ERROR] Failed marshaling structured feedback in category action: %s", err)
			respBody = []byte(`{"success": false, "reason": "Failed marshaling structured feedback. Contact"}`)
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		resp.WriteHeader(400)
		resp.Write(jsonBytes)
		return jsonBytes, nil
	} else {
		if len(foundCategory.Name) > 0 {
			log.Printf("[DEBUG] Found app for category %s: %s (%s)", foundAppType, foundCategory.Name, foundCategory.ID)
		}
	}

	selectedApp := shuffle.WorkflowApp{}
	selectedCategory := shuffle.AppCategory{}
	selectedAction := shuffle.WorkflowAppAction{}

	//RunAiQuery(systemMessage, userMessage)

	partialMatch := true
	availableLabels := []string{}

	matchName := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value.AppName)), " ", "_")
	for _, app := range newapps {
		if app.Name == "" || len(app.Categories) == 0 {
			continue
		}

		// If we HAVE an app as a category already
		if len(matchName) == 0 {
			availableLabels = []string{}
			if len(app.Categories) == 0 {
				continue
			}

			if strings.ToLower(app.Categories[0]) != value.Category {
				continue
			}

			// Check if the label is in the action
			for _, action := range app.Actions {
				if len(action.CategoryLabel) == 0 {
					continue
				}

				availableLabels = append(availableLabels, action.CategoryLabel[0])

				if len(value.ActionName) > 0 && action.Name != value.ActionName {
					continue
				}

				if strings.ReplaceAll(strings.ToLower(action.CategoryLabel[0]), " ", "_") == value.Label {
					selectedAction = action

					log.Printf("[INFO] Found label %s in app %s. ActionName: %s", value.Label, app.Name, action.Name)
					break
				}
			}

			//log.Printf("[DEBUG] Found app with category %s", value.Category)
			selectedApp = app
			if len(selectedAction.Name) > 0 {
				if debug { 
					log.Printf("[DEBUG] Selected app %s (%s) with action %s", selectedApp.Name, selectedApp.ID, selectedAction.Name)
				}

				// FIXME: Don't break on the first one?
				//break
				availableLabels = []string{}
			}

			if foundCategory.ID == app.ID {
				break
			}

		} else {
			appName := strings.TrimSpace(strings.ReplaceAll(strings.ToLower(app.Name), " ", "_"))

			// If we DONT have a category app already
			if app.ID == matchName || appName == matchName {
				selectedApp = app
				//log.Printf("[DEBUG] Found app - checking label: %s vs %s (%s)", app.Name, value.AppName, app.ID)

				selectedAction, selectedCategory, availableLabels = GetActionFromLabel(ctx, app, value.Label, true, value.Fields, 0)
				partialMatch = false

				break

				// Finds a random match, but doesn't break in case it finds exact
			} else if selectedApp.ID == "" && len(matchName) > 0 && (strings.Contains(appName, matchName) || strings.Contains(matchName, appName)) {
				selectedApp = app

				log.Printf("[WARNING] Set selected app to PARTIAL match %s (%s) for input %s", selectedApp.Name, selectedApp.ID, value.AppName)
				selectedAction, selectedCategory, availableLabels = GetActionFromLabel(ctx, app, value.Label, true, value.Fields, 0)

				partialMatch = true
			}
		}
	}

	// In case we wanna use this to get good matches
	_ = partialMatch

	if len(selectedApp.ID) == 0 {
		log.Printf("[WARNING] Couldn't find app with ID or name '%s' active in org %s (%s)", value.AppName, user.ActiveOrg.Name, user.ActiveOrg.Id)
		failed := true
		if shuffle.GetProject().Environment == "cloud" {
			foundApp, err := shuffle.HandleAlgoliaAppSearch(ctx, value.AppName)
			if err != nil {
				log.Printf("[ERROR] Failed getting app with ID or name '%s' in category action: %s", value.AppName, err)
			} else if err == nil && len(foundApp.ObjectID) > 0 {
				log.Printf("[INFO] Found app %s (%s) for name %s in Algolia", foundApp.Name, foundApp.ObjectID, value.AppName)

				if !standalone {
					tmpApp, err := shuffle.GetApp(ctx, foundApp.ObjectID, user, false)

					if err == nil {
						selectedApp = *tmpApp
						failed = false

						//log.Printf("[DEBUG] Got app %s with %d actions", selectedApp.Name, len(selectedApp.Actions))
						selectedAction, selectedCategory, availableLabels = GetActionFromLabel(ctx, selectedApp, value.Label, true, value.Fields, 0)
					}
				}
			} else {
				log.Printf("[DEBUG] Found app with ID or name '%s' in Algolia: %#v", value.AppName, foundApp)
			}
		}

		if failed {
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed finding an app with the name or ID '%s'. Explain more clearly what app you would like to run with."}`, value.AppName))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, fmt.Errorf("Failed finding an app with the name or ID '%s'", value.AppName)
		}
	}

	// Section for mapping fields to automatic translation from previous runs
	fieldHash := ""
	fieldFileFound := false
	fieldFileContentMap := map[string]interface{}{}

	discoverFile := ""
	if len(value.Fields) > 0 {
		sortedKeys := []string{}
		for _, field := range value.Fields {
			sortedKeys = append(sortedKeys, field.Key)
		}

		sort.Strings(sortedKeys)
		newFields := []shuffle.Valuereplace{}
		for _, key := range sortedKeys {
			for _, field := range value.Fields {
				if field.Key == key {
					newFields = append(newFields, field)
					break
				}
			}
		}

		value.Fields = newFields

		// Md5 based on sortedKeys. Could subhash key search work?
		mappedString := fmt.Sprintf("%s %s-%s", selectedApp.ID, value.Label, strings.Join(sortedKeys, ""))
		fieldHash = fmt.Sprintf("%x", md5.Sum([]byte(mappedString)))
		discoverFile = fmt.Sprintf("file_%s", fieldHash)

		// Ensuring location is proper during load
		if standalone {
			discoverFile = fmt.Sprintf("translation_output/%s", discoverFile)
		}

		if debug {
			log.Printf("[DEBUG] Loading content mapping for fields '%s' with hash '%s'. Filepath: '%s'", strings.Join(sortedKeys, ","), fieldHash, discoverFile)
		}

		file, err := shuffle.GetFileSingul(ctx, discoverFile)
		if err != nil {
			if debug {
				log.Printf("[ERROR] Problem with getting field file '%s' in category action autorun: %s", discoverFile, err)
			}
		} else {
			//log.Printf("[DEBUG] Found translation file in category action: %#v. Status: %s. Category: %s", file.Id, file.Status, file.Namespace)

			if file.Status == "active" {

				fieldFileFound = true

				//log.Printf("[DEBUG] File found: %s", file.Filename)

				fileContent, err := shuffle.GetFileContentSingul(ctx, file, nil)
				if err != nil {
					log.Printf("[ERROR] Failed getting file content in category action: %s", err)
					fieldFileFound = false
				}

				//log.Printf("Output content: %#v", string(fileContent))
				err = json.Unmarshal(fileContent, &fieldFileContentMap)
				if err != nil {
					log.Printf("[ERROR] Failed unmarshaling file content in category action: %s", err)
				}
			} else {
				log.Printf("[ERROR] File %s (%s) not active in category action: %s", file.Filename, file.Id, file.Status)
			}
		}
	}

	if strings.Contains(strings.ToLower(strings.Join(selectedApp.ReferenceInfo.Triggers, ",")), "webhook") {
		log.Printf("[INFO] App %s (%s) has a webhook trigger as default. Setting available labels to Webhook", selectedApp.Name, selectedApp.ID)
		availableLabels = append(availableLabels, "Webhook")

		if len(selectedAction.Name) == 0 {
			//log.Printf("\n\n[DEBUG] Due to webhook in app %s (%s), we don't need an action. Should not return\n\n", selectedApp.Name, selectedApp.ID)
		}
	}

	if len(selectedAction.Name) == 0 && value.Label != "discover_app" {
		log.Printf("[WARNING] Couldn't find the label '%s' in app '%s'.", value.Label, selectedApp.Name)

		//selectedApp := AutofixAppLabels(selectedApp, value.Label)

		if value.Label != "app_authentication" && value.Label != "authenticate_app" && value.Label != "discover_app" {
			respBody = []byte(fmt.Sprintf(`{"success": false, "app_id": "%s", "reason": "Failed finding action '%s' labeled in app '%s'. If this is wrong, please suggest a label by finding the app in Shuffle (%s), OR contact support@shuffler.io and we can help with labeling."}`, selectedApp.ID, value.Label, strings.ReplaceAll(selectedApp.Name, "_", " "), fmt.Sprintf("https://shuffler.io/apis/%s", selectedApp.ID)))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, fmt.Errorf("Failed finding action '%s' labeled in app '%s'", value.Label, selectedApp.Name)
		} else {
			//log.Printf("[DEBUG] NOT sending back due to label %s", value.Label)
		}
	}

	discoveredCategory := foundAppType
	if len(selectedApp.Categories) > 0 {
		discoveredCategory = selectedApp.Categories[0]
	}

	if len(selectedCategory.Name) > 0 {
		if len(selectedCategory.RequiredFields) == 0 {
			for _, param := range selectedAction.Parameters {
				if !param.Required {
					continue
				}

				if param.Name == "url" {
					log.Printf("[DEBUG] Found URL in required fields: %s - %s", param.Name, param.Value)
					continue
				}

				selectedCategory.RequiredFields[param.Name] = []string{param.Name}
			}
		}
	}

	// Need translation here, now that we have the action
	// Should just do an app injection into the workflow?
	foundFields := 0
	missingFields := []string{}
	baseFields := []string{}

	_ = foundFields
	for innerLabel, labelValue := range selectedCategory.RequiredFields {
		if strings.ReplaceAll(strings.ToLower(innerLabel), " ", "_") != value.Label {
			continue
		}

		baseFields = labelValue
		missingFields = labelValue
		for _, field := range value.Fields {
			if shuffle.ArrayContains(labelValue, field.Key) {
				// Remove from missingFields
				missingFields = shuffle.RemoveFromArray(missingFields, field.Key)

				foundFields += 1
			}
		}

		break
	}

	if len(missingFields) > 0 {
		for _, missingField := range missingFields {
			if missingField != "body" {
				continue
			}

			fixedBody, err := shuffle.UpdateActionBody(selectedAction)
			if err != nil {
				log.Printf("[ERROR] Failed getting correct action body for %s: %s", selectedAction.Name, err)
				continue
			}

			log.Printf("[DEBUG] GOT BODY: %#v", fixedBody)
		}
	}

	// FIXME: Check if ALL fields for the target app can be fullfiled
	// E.g. for Jira: Org_id is required.
	_ = baseFields

	/*











	 */

	// Uses the thread to continue generating in the same workflow
	foundWorkflow := &shuffle.Workflow{}
	parentWorkflowId := uuid.NewV4().String()
	if len(value.WorkflowId) > 0 && !strings.HasPrefix(value.WorkflowId, "thread") {
		parentWorkflowId = value.WorkflowId

		// Get the workflow
		foundWorkflow, err = shuffle.GetWorkflow(ctx, parentWorkflowId)
		if err != nil {
			log.Printf("[WARNING] Failed getting workflow %s: %s", parentWorkflowId, err)
		}
	}

	if len(threadId) > 0 && strings.HasPrefix(threadId, "thread") {
		// Set the thread ID in cache
		err = shuffle.SetCache(ctx, threadId, []byte(parentWorkflowId), 30)
		if err != nil {
			log.Printf("[WARNING] Failed setting cache for thread ID %s: %s", threadId, err)
		}
	}

	startId := uuid.NewV4().String()
	workflowName := fmt.Sprintf("Category action: %s", selectedAction.Name)
	if len(value.Query) > 0 {
		workflowName = value.Query
	}

	parentWorkflow := shuffle.Workflow{
		ID:          parentWorkflowId,
		Name:        workflowName,
		Description: fmt.Sprintf("Category action: %s. This is a workflow generated and ran by ShuffleGPT. More here: https://shuffler.io/chat", selectedAction.Name),
		Generated:   true,
		Hidden:      true,
		Start:       startId,
		OrgId:       user.ActiveOrg.Id,
	}

	if len(foundWorkflow.ID) > 0 {
		parentWorkflow = *foundWorkflow
		parentWorkflow.Start = startId

		// Remove is startnode from previous nodes
		newActions := []shuffle.Action{}
		for actionIndex, action := range parentWorkflow.Actions {
			if len(selectedAction.Name) > 0 && action.Name == selectedAction.Name {
				//log.Printf("[DEBUG] Found action %s, setting as start node", action.Name)
				continue
			}

			if action.IsStartNode {
				parentWorkflow.Actions[actionIndex].IsStartNode = false
			}

			newActions = append(newActions, action)
		}

		// Remove any node with the same name
		parentWorkflow.Actions = newActions
	}

	environment := "Cloud"
	// FIXME: Make it dynamic based on the default env
	if shuffle.GetProject().Environment != "cloud" {
		environment = "Shuffle"
	}

	// Get the environments for the user and choose the default
	if !standalone { 
		environments, err := shuffle.GetEnvironments(ctx, user.ActiveOrg.Id)
		if err == nil {
			defaultEnv := ""

			foundEnv := strings.TrimSpace(strings.ToLower(value.Environment))
			for _, env := range environments {
				if env.Archived {
					continue
				}

				if env.Default {
					defaultEnv = env.Name
				}

				envName := strings.TrimSpace(strings.ToLower(env.Name))
				if len(value.Environment) > 0 && envName == foundEnv {
					environment = env.Name
				}
			}

			if len(environment) == 0 && len(defaultEnv) > 0 {
				environment = defaultEnv
			}
		}
	}

	auth := []shuffle.AppAuthenticationStorage{}
	foundAuthenticationId := value.AuthenticationId
	if len(foundAuthenticationId) == 0 {
		// 1. Get auth
		// 2. Append the auth
		// 3. Run!

		if standalone { 
			auth, err = GetLocalAuth()
			if err != nil {
				if debug { 
					log.Printf("[WARNING] Failed getting local auth: %s", err)
				}
			}

		} else {
			auth, err = shuffle.GetAllWorkflowAppAuth(ctx, user.ActiveOrg.Id)
			if err != nil {
				log.Printf("[WARNING] Failed getting auths for org %s: %s", user.ActiveOrg.Id, err)
			} 
		}

		if err == nil {
			latestEdit := int64(0)
			for _, auth := range auth {
				// Check if the app name or ID is correct
				if auth.App.Name != selectedApp.Name && auth.App.ID != selectedApp.ID {
					continue
				}

				// Taking whichever valid is last
				if auth.Validation.Valid == true {
					foundAuthenticationId = auth.Id
					break
				}

				// Check if the auth is valid
				if auth.Edited < latestEdit {
					continue
				}

				foundAuthenticationId = auth.Id
				latestEdit = auth.Edited
			}
		}
	}

	if len(foundAuthenticationId) > 0 {
		if len(auth) == 0 {

			if standalone { 
				auth, err = GetLocalAuth()
			} else {
				auth, err = shuffle.GetAllWorkflowAppAuth(ctx, user.ActiveOrg.Id)
				if err != nil {
					log.Printf("[DEBUG] Failed loading auth: %s", err)
				}
			}
		}

		// Fixes an issue where URL replacing from auth doesn't work
		// due to a value already existing
		foundUrl := ""
		urlIndex := -1
		for paramIndex, param := range selectedAction.Parameters {
			if param.Name == "url" {
				foundUrl = param.Value
				urlIndex = paramIndex
				break
			}
		}

		if len(foundUrl) > 0 && urlIndex >= 0 {
			for _, foundAuth := range auth {
				if foundAuth.Id != foundAuthenticationId {
					continue
				}

				// Replaces if URL is in the authentication, as it should be
				// replaced at a later point
				for _, field := range foundAuth.Fields {
					if field.Key == "url" {
						selectedAction.Parameters[urlIndex].Value = ""
						break
					}
				}
			}
		}
	} else {
		//log.Printf("[WARNING] Couldn't find auth for app %s in org %s (%s)", selectedApp.Name, user.ActiveOrg.Name, user.ActiveOrg.Id)

		requiresAuth := false
		for _, action := range selectedApp.Actions {
			//if value.SkipAuthentication {
			//	break
			//}

			for _, param := range action.Parameters {
				if param.Configuration {
					requiresAuth = true
					break
				}
			}

			if requiresAuth {
				break
			}
		}

		if !requiresAuth {
			//log.Printf("\n\n[ERROR] App '%s' doesn't require auth\n\n", selectedApp.Name)
		} else {
			// Reducing size drastically as it isn't really necessary
			selectedApp.Actions = []shuffle.WorkflowAppAction{}
			//selectedApp.Authentication = Authentication{}
			selectedApp.ChildIds = []string{}
			selectedApp.SmallImage = ""
			structuredFeedback := shuffle.StructuredCategoryAction{
				Success:  false,
				Action:   "app_authentication",
				Category: discoveredCategory,
				Reason:   fmt.Sprintf("Authenticate %s first.", selectedApp.Name),
				Label:    value.Label,
				Apps: []shuffle.WorkflowApp{
					selectedApp,
				},
				ApiDebuggerUrl:  fmt.Sprintf("https://shuffler.io/apis/%s", selectedApp.ID),
				AvailableLabels: availableLabels,
			}

			// Check for user agent including shufflepy
			useragent := request.Header.Get("User-Agent")
			if strings.Contains(strings.ToLower(useragent), "shufflepy") {
				structuredFeedback.Apps = []shuffle.WorkflowApp{}

				// Find current domain from the url
				currentUrl := fmt.Sprintf("%s://%s", request.URL.Scheme, request.URL.Host)
				if shuffle.GetProject().Environment == "cloud" {
					currentUrl = "https://shuffler.io"
				}

				// FIXME: Implement this. Uses org's auth
				orgAuth := org.OrgAuth.Token

				structuredFeedback.Reason = fmt.Sprintf("Authenticate here: %s/appauth?app_id=%s&auth=%s", currentUrl, selectedApp.ID, orgAuth)
			}

			jsonFormatted, err := json.MarshalIndent(structuredFeedback, "", "    ")
			if err != nil {
				log.Printf("[WARNING] Failed marshalling structured feedback: %s", err)
				respBody = []byte(`{"success": false, "reason": "Failed formatting your data"}`)
				resp.WriteHeader(500)
				resp.Write(respBody)
				return respBody, err
			}

			// Replace \u0026 with &
			jsonFormatted = bytes.Replace(jsonFormatted, []byte("\\u0026"), []byte("&"), -1)

			resp.WriteHeader(400)
			resp.Write(jsonFormatted)
			return jsonFormatted, nil
		}
	}

	// Send back with SUCCESS as we already have an authentication
	// Use the labels to show what Jira can do
	if value.Label == "app_authentication" || value.Label == "authenticate_app" || value.Label == "discover_app" {
		ifSuccess := true
		reason := fmt.Sprintf("Please authenticate with %s", selectedApp.Name)

		// Find out if it should re-authenticate or not
		for _, field := range value.Fields {
			if (field.Key == "re-authenticate" || field.Key == "reauthenticate") && field.Value == "true" {
				ifSuccess = false
				reason = fmt.Sprintf("Please re-authenticate with %s by clicking the button below!", selectedApp.Name)
			}
		}

		selectedApp.Actions = []shuffle.WorkflowAppAction{}
		//selectedApp.Authentication = Authentication{}
		selectedApp.ChildIds = []string{}
		selectedApp.SmallImage = ""
		structuredFeedback := shuffle.StructuredCategoryAction{
			Success:  ifSuccess,
			Action:   "app_authentication",
			Category: discoveredCategory,
			Reason:   reason,
			Apps: []shuffle.WorkflowApp{
				selectedApp,
			},
			AvailableLabels: availableLabels,
			ApiDebuggerUrl:  fmt.Sprintf("https://shuffler.io/apis/%s", selectedApp.ID),
		}

		// marshalled
		jsonFormatted, err := json.Marshal(structuredFeedback)
		if err != nil {
			log.Printf("[WARNING] Failed marshalling structured feedback: %s", err)
			respBody = []byte(`{"success": false, "reason": "Failed formatting your data"}`)
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		resp.WriteHeader(400)
		resp.Write(jsonFormatted)
		return jsonFormatted, nil
	}

	if !standalone && !value.SkipWorkflow {
		log.Printf("\n\n[INFO] Adding workflow %s\n\n", parentWorkflow.ID)
	}

	// Now start connecting it to the correct app (Jira?)
	label := selectedAction.Name
	if strings.HasPrefix(label, "get_list") {
		// Remove get_ at the start
		label = strings.Replace(label, "get_list", "list", 1)
	}

	step := int64(0)
	if value.Step >= 1 {
		step = value.Step
	} else {
		step = int64(len(parentWorkflow.Actions) + 1)
	}

	// FIXME: Why are we making a second entry here again?
	secondAction := shuffle.Action{
		Name:        selectedAction.Name,
		Label:       selectedAction.Name,
		AppName:     selectedApp.Name,
		AppVersion:  selectedApp.AppVersion,
		AppID:       selectedApp.ID,
		Environment: environment,
		IsStartNode: true,
		ID:          uuid.NewV4().String(),
		Parameters:  []shuffle.WorkflowAppActionParameter{},
		Position: shuffle.Position{
			X: 0.0 + float64(step*300.0),
			Y: 100.0,
		},
		AuthenticationId: foundAuthenticationId,
		IsValid:          true,
		//ReferenceUrl:     refUrl,
	}

	// Ensuring it gets overwritten
	startNode := shuffle.Action{
		Position: shuffle.Position{
			X: 100000,
		},
	}

	if step > 0 {
		// Should find nearest neighor to the left, and then add a branch from it to this one
		// If it's a GENERATED app, add a condition to the branch checking if the output status is < 300

		nearestXNeighbor := shuffle.Action{}
		for _, action := range parentWorkflow.Actions {
			if action.Position.X > nearestXNeighbor.Position.X {
				nearestXNeighbor = action
			}

			// Find farthest to the left
			if action.Position.X < startNode.Position.X {
				startNode = action
			}
		}

		if nearestXNeighbor.ID != "" {

			//Conditions    []Condition `json:"conditions" datastore: "conditions"`
			parentWorkflow.Branches = append(parentWorkflow.Branches, shuffle.Branch{
				Label: "Validating",

				SourceID:      nearestXNeighbor.ID,
				DestinationID: secondAction.ID,
				ID:            uuid.NewV4().String(),

				Conditions: []shuffle.Condition{
					shuffle.Condition{
						Source: shuffle.WorkflowAppActionParameter{
							Name:    "source",
							Value:   fmt.Sprintf("$%s.status", strings.ReplaceAll(nearestXNeighbor.Label, " ", "_")),
							Variant: "STATIC_VALUE",
						},
						Condition: shuffle.WorkflowAppActionParameter{
							Name:    "condition",
							Value:   "less than",
							Variant: "STATIC_VALUE",
						},
						Destination: shuffle.WorkflowAppActionParameter{
							Name:    "destination",
							Value:   "300",
							Variant: "STATIC_VALUE",
						},
					},
				},
			})
		}
	}

	if len(selectedAction.RequiredBodyFields) == 0 && len(selectedCategory.RequiredFields) > 0 {
		// Check if the required fields are present
		//selectedAction.RequiredBodyFields = selectedCategory.RequiredFields
		for labelName, requiredFields := range selectedCategory.RequiredFields {
			newName := strings.ReplaceAll(strings.ToLower(labelName), " ", "_")
			if newName != value.Label {
				continue
			}

			for _, requiredField := range requiredFields {
				selectedAction.RequiredBodyFields = append(selectedAction.RequiredBodyFields, requiredField)
			}
		}
	}

	// Check if the organisation has a specific set of parameters for this action, mapped to the following fields:
	selectedAction.AppID = selectedApp.ID
	selectedAction.AppName = selectedApp.Name
	selectedAction = GetOrgspecificParameters(ctx, *org, selectedAction)

	//log.Printf("[DEBUG] Required bodyfields: %#v", selectedAction.RequiredBodyFields)
	handledRequiredFields := []string{}
	missingFields = []string{}

	for _, param := range selectedAction.Parameters {
		// Optional > Required
		fieldChanged := false
		for _, field := range value.OptionalFields {
			if strings.ReplaceAll(strings.ToLower(field.Key), " ", "_") == strings.ReplaceAll(strings.ToLower(param.Name), " ", "_") {
				param.Value = field.Value
				fieldChanged = true

				//handledFields = append(handledFields, field.Key)
			}
		}

		// Parse input params into the field
		for _, field := range value.Fields {
			//log.Printf("%s & %s", strings.ReplaceAll(strings.ToLower(field.Key), " ", "_"), strings.ReplaceAll(strings.ToLower(param.Name), " ", "_"))
			if strings.ReplaceAll(strings.ToLower(field.Key), " ", "_") == strings.ReplaceAll(strings.ToLower(param.Name), " ", "_") {
				param.Value = field.Value
				fieldChanged = true

				handledRequiredFields = append(handledRequiredFields, field.Key)
			}
		}

		if param.Name == "body" {

			// FIXME: Look for key:values and inject values into them
			// This SHOULD be just a dumb injection of existing value.Fields & value.OptionalFields for now with synonyms, but later on it should be a more advanced (use schemaless & cross org referencing)
			if len(param.Example) > 0 {
				param.Value = param.Example
			} else {
				//param.Value = ""
				log.Printf("[DEBUG] Found body param. Validating: %#v", param)
				if !fieldChanged {
					log.Printf("\n\nBody not filled yet. Should fill it in (somehow) based on the existing input fields.\n\n")
				}

				param.Required = true
			}
		}

		if param.Required && len(param.Value) == 0 && !param.Configuration {
			missingFields = append(missingFields, param.Name)
		}

		secondAction.Parameters = append(secondAction.Parameters, param)
	}

	if len(handledRequiredFields) < len(value.Fields) {
		// Compare which ones are not handled from value.Fields

		/*
		log.Printf("[DEBUG] handledRequiredFields: %+v", handledRequiredFields)
		for _, field := range value.Fields {
			log.Printf("[DEBUG] fields provided: %s - %s", field.Key, field.Value)
		}

		for _, field := range handledRequiredFields {
			log.Printf("[DEBUG] fields required (2): %s", field)
		}

		for missingIndex, missingField := range missingFields {
			log.Printf("[DEBUG] missingField %d. %s", missingIndex, missingField)
		}
		*/

		for _, field := range value.Fields {
			if !shuffle.ArrayContains(handledRequiredFields, field.Key) {
				missingFields = append(missingFields, field.Key)
			}
		}

		/*
		if debug { 
			log.Printf("[DEBUG] Not all required fields were handled (1). Missing: %#v. Should force use of all fields? Handled fields: %3v", missingFields, handledRequiredFields)
		}
		*/
	}

	// Send request to /api/v1/conversation with this data
	baseUrl := fmt.Sprintf("https://shuffler.io")
	if len(os.Getenv("BASE_URL")) > 0 {
		baseUrl = fmt.Sprintf("%s", os.Getenv("BASE_URL"))
	}

	if len(os.Getenv("SHUFFLE_CLOUDRUN_URL")) > 0 {
		baseUrl = fmt.Sprintf("%s", os.Getenv("SHUFFLE_CLOUDRUN_URL"))
	}

	client := shuffle.GetExternalClient(baseUrl)

	selectedAction.AppName = selectedApp.Name
	selectedAction.AppID = selectedApp.ID
	selectedAction.AppVersion = selectedApp.AppVersion

	for _, missing := range missingFields {
		// FIXME: ONLY body is handled well so far?
		if missing != "body" {
			continue
		}

		fixedBody, err := shuffle.UpdateActionBody(selectedAction)
		if err != nil {
			log.Printf("[ERROR] Failed getting correct action body for %s: %s", selectedAction.Name, err)
			continue
		}

		// Remove newlines
		fixedBody = strings.ReplaceAll(fixedBody, "\n", "")

		if debug { 
			log.Printf("[DEBUG] GOT BODY: %#v", fixedBody)
		}

		if len(fixedBody) > 0 {
			bodyFound := false
			for paramIndex, param := range selectedAction.Parameters {
				if param.Name != "body" {
					continue
				}

				bodyFound = true
				//selectedAction.Parameters[paramIndex].Example = fixedBody
				selectedAction.Parameters[paramIndex].Value = fixedBody
				selectedAction.Parameters[paramIndex].Tags = append(selectedAction.Parameters[paramIndex].Tags, "generated")
			}

			if !bodyFound {
				selectedAction.Parameters = append(selectedAction.Parameters, shuffle.WorkflowAppActionParameter{
					Name:  "body",
					Value: fixedBody,
					//Example: fixedBody,
					Tags: []string{"generated"},
				})
			}

			missingFields = shuffle.RemoveFromArray(missingFields, "body")
		}

		break
	}

	// Finds WHERE in the destination to put the input data
	// Loops through input fields, then takes the data from them
	if len(fieldFileContentMap) > 0 {
		if debug { 
			log.Printf("[DEBUG] Found file content map (Pre Reverse Schemaless): %#v", fieldFileContentMap)
		}

		for key, mapValue := range fieldFileContentMap {
			if _, ok := mapValue.(string); !ok {
				log.Printf("[WARNING] Value for key %s is not a string: %#v", key, mapValue)
				continue
			}

			mappedFieldSplit := strings.Split(mapValue.(string), ".")
			if len(mappedFieldSplit) == 0 {
				log.Printf("[WARNING] Failed splitting value for key %s: %#v", key, mapValue)
				continue
			}

			// Finds the location
			for _, field := range value.Fields {
				if field.Key != key {
					continue
				}

				mapValue = field.Value
				break
			}

			// Check if the key exists in the parameters
			for paramIndex, param := range selectedAction.Parameters {
				if param.Name != mappedFieldSplit[0] {
					continue
				}

				foundIndex = paramIndex
				if param.Name == "queries" {
					if len(param.Value) == 0 {
						selectedAction.Parameters[paramIndex].Value = fmt.Sprintf("%s=%s", key, mapValue.(string))
					} else {
						selectedAction.Parameters[paramIndex].Value = fmt.Sprintf("%s&%s=%s", param.Value, key, mapValue.(string))
					}

					missingFields = shuffle.RemoveFromArray(missingFields, key)


				} else if param.Name == "body" {
					mapToSearch := map[string]interface{}{}
					err := json.Unmarshal([]byte(param.Value), &mapToSearch)
					if err != nil {
						log.Printf("[ERROR] Failed unmarshalling body for file content: %s. Body: %#v", err, string(param.Value))
						continue
					}

					// Finds where in the body the value should be placed
					location := strings.Join(mappedFieldSplit[1:], ".")
					//log.Printf("\n\n\nHandling mapping of '%s' -> '%s'\n\n\n", mapValue.(string), location)

					outputMap := schemaless.MapValueToLocation(mapToSearch, location, mapValue.(string))

					// Marshal back to JSON
					marshalledMap, err := json.MarshalIndent(outputMap, "", "    ")
					if err != nil {
						log.Printf("[WARNING] Failed marshalling body for file content: %s", err)
					} else {
						selectedAction.Parameters[paramIndex].Value = string(marshalledMap)
						missingFields = shuffle.RemoveFromArray(missingFields, key)
					}

					if debug { 
						log.Printf("[DEBUG] OLD BODY: \n\n%s\n\nNEW BODY: \n\n%s", param.Value, string(marshalledMap))
					}


				} else {
					// Hmm
					selectedAction.Parameters[paramIndex].Value = mapValue.(string)

					if debug {
						log.Printf("[DEBUG] Found value for key (NOT body/queries) %s: %s -- %+v... PARAM NAME: %#v", key, mapValue, missingFields, param.Name)
					}

					missingFields = shuffle.RemoveFromArray(missingFields, key)
					missingFields = shuffle.RemoveFromArray(missingFields, selectedAction.Parameters[paramIndex].Name)

				}
					
				secondAction.Parameters = selectedAction.Parameters
				break
			}
		}
	}

	// AI fallback mechanism to handle missing fields
	// This is in case some fields are not sent in properly
	orgId := ""
	authorization := ""
	optionalExecutionId := ""
	if len(missingFields) > 0 {
		if debug {
			log.Printf("[DEBUG] Missing fields for action: %#v. This means the current translation may be missing or wrong. Setting fieldFileFound back to false and re-setting it at the end", missingFields)
		}

		fieldFileFound = false  

		formattedQueryFields := []string{}
		for _, missing := range missingFields {

			for _, field := range value.Fields {

				if field.Key != missing {
					continue
				}

				formattedQueryFields = append(formattedQueryFields, fmt.Sprintf("%s=%s", field.Key, field.Value))
				break
			}
		}


		formattedQuery := fmt.Sprintf("Use the fields '%s' with app %s to '%s'.", strings.Join(formattedQueryFields, "&"), strings.ReplaceAll(selectedApp.Name, "_", " "), strings.ReplaceAll(value.Label, "_", " "))

		newQueryInput := shuffle.QueryInput{
			Query:        formattedQuery,
			//OutputFormat: "action", 		   // To run the action (?)
			OutputFormat: "action_parameters", 

			//Label: value.Label,
			Category: value.Category,

			AppId:      selectedApp.ID,
			AppName:    selectedApp.Name,
			ActionName: selectedAction.Name,
			Parameters: selectedAction.Parameters,
		}

		responseBody, err := GetActionAIResponseWrapper(ctx, newQueryInput)
		if err != nil {
			log.Printf("[WARNING] Failed getting AI response: %s", err)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed getting AI response. Contact support."}`))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		//log.Printf("[DEBUG] GOT RESPONSE BODY (params): %#v", string(responseBody))

		//log.Printf("\n\n[DEBUG] TRANSLATED REQUEST RETURNED: %s\n\n", string(responseBody))
		if strings.Contains(string(responseBody), `"success": false`) {
			log.Printf("[WARNING] Failed running app %s (%s). Contact support.", selectedAction.Name, selectedAction.AppID)
			resp.WriteHeader(500)
			resp.Write(responseBody)
			return responseBody, err
		}

		// Unmarshal responseBody back to
		newSecondAction := shuffle.Action{}
		err = json.Unmarshal(responseBody, &newSecondAction)
		if err != nil {
			log.Printf("[WARNING] Failed unmarshalling body for execute generated workflow: %s %+v", err, string(responseBody))
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed parsing app response. Contact support if this persists."}`))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		secondAction.LargeImage = ""
		missingFields = []string{}
		for _, param := range newSecondAction.Parameters {
			if param.Configuration {
				continue
			}

			for paramIndex, originalParam := range secondAction.Parameters {
				if originalParam.Name != param.Name {
					continue
				}

				if len(param.Value) > 0 && param.Value != originalParam.Value {
					secondAction.Parameters[paramIndex].Value = param.Value
					secondAction.Parameters[paramIndex].Tags = param.Tags
				}

				//log.Printf("[INFO] Required - Key: %s, Value: %#v", param.Name, param.Value)
				if originalParam.Required && len(secondAction.Parameters[paramIndex].Value) == 0 {
					missingFields = append(missingFields, param.Name)
				}

				break
			}
		}
	}

	// Runs individual apps, one at a time
	if value.SkipWorkflow {

		if len(missingFields) > 0 {
			log.Printf("[WARNING] Not all required fields were found in category action (2). Want: %#v in action %s", missingFields, selectedAction.Name)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Not all required fields are set", "label": "%s", "missing_fields": "%s", "action": "%s", "api_debugger_url": "%s"}`, value.Label, strings.Join(missingFields, ","), selectedAction.Name, fmt.Sprintf("https://shuffler.io/apis/%s", selectedApp.ID)))
			resp.WriteHeader(400)
			resp.Write(respBody)
			return respBody, errors.New("Not all required fields were found")
		}

		// FIXME: Make a check for IF we have filled in all fields or not
		for paramIndex, param := range secondAction.Parameters {
			secondAction.Parameters[paramIndex].Example = ""
			if param.Name == "headers" {
				// for now
				secondAction.Parameters[paramIndex].Value = "Content-Type: application/json"
			}
		}

		//log.Printf("[DEBUG] App authentication: %#v", secondAction.AuthenticationId)
		preparedAction, err := json.Marshal(secondAction)
		if err != nil {
			log.Printf("[WARNING] Failed marshalling action in category run for app %s: %s", secondAction.AppID, err)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed marshalling action"}`))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		// This is due to needing debug capabilities

		if !standalone && len(request.Header.Get("Authorization")) > 0 {
			tmpAuth := request.Header.Get("Authorization")

			if strings.HasPrefix(tmpAuth, "Bearer") {
				tmpAuth = strings.Replace(tmpAuth, "Bearer ", "", 1)
			}

			authorization = tmpAuth
		}

		// The app run url to use. Default delete is false
		shouldDelete := "false"
		apprunUrl := fmt.Sprintf("%s/api/v1/apps/%s/run?delete=%s", baseUrl, secondAction.AppID, shouldDelete)

		if !standalone && len(request.Header.Get("Authorization")) == 0 && len(request.URL.Query().Get("execution_id")) > 0 && len(request.URL.Query().Get("authorization")) > 0 {
			apprunUrl = fmt.Sprintf("%s&execution_id=%s&authorization=%s", apprunUrl, request.URL.Query().Get("execution_id"), request.URL.Query().Get("authorization"))

			authorization = request.URL.Query().Get("authorization")
			optionalExecutionId = request.URL.Query().Get("execution_id")
		}

		if len(value.OrgId) > 0 {
			apprunUrl = fmt.Sprintf("%s&org_id=%s", apprunUrl, value.OrgId)
		}

		additionalInfo := ""
		inputQuery := ""
		originalAppname := selectedApp.Name

		// Add "execution-url" header with a full link
		//resp.Header().Add("execution-url", fmt.Sprintf("/workflows/%s?execution_id=%s", parentWorkflow.ID, optionalExecutionId))
		resp.Header().Add("x-apprun-url", apprunUrl)

		// Runs attempts up to X times
		maxAttempts := 5

		//if !standalone { 
		//	log.Printf("[DEBUG][AI] Sending single API run execution to %s", apprunUrl)
		//}

		marshalledBody := []byte(`{"success": false, "reason": "Action didn't run yet. This is pre-validation."}`)
		httpOutput := shuffle.HTTPOutput{
			Success: false,
			Status: 400,
		}
		httpParseErr := errors.New("Starting error with no status: ")
		startTime := time.Now()

		// The request that goes to the CORRECT app
		// SubflowData{} -> HTTPOutput in .Result

		apprunBody := []byte("{\"success\": true,\"result\": \"{\\\"success\\\": true,\\\"status\\\": 400, \\\"body\\\": \\\"Not all fields have been filled. Ensure the below are inputted in the right field, in the right format.\\\\n")
		for i := 0; i < maxAttempts; i++ {

			// This is an autocorrective measure to ensure ALL fields
			// have been filled in according to the input from the user.
			// This can be disabled with the environment variable:
			// SKIP_VALIDATION=true
			missingBodyParams := validatePreparedActionHasFields(preparedAction, value.Fields)
			if len(missingBodyParams) > 0 {
				fieldFileFound = false
				allMissingFields := []string{}

				for key, value := range missingBodyParams {
					allMissingFields = append(allMissingFields, key)

					apprunBody = []byte(fmt.Sprintf("%s%s=%s\\\\n", string(apprunBody), key, value))

				}

				log.Printf("[ERROR] Problem with missing fields: %#v. This means you MAY get the wrong outcome. Rerunning translations to ensure all fields are included.", strings.Join(allMissingFields, ","))

				apprunBody = []byte(fmt.Sprintf("%s\\\"}\"}", apprunBody))

			} else {
				if standalone { 
					unmarshalledAction := shuffle.Action{}
					err = json.Unmarshal(preparedAction, &unmarshalledAction)
					if err != nil {
						log.Printf("[ERROR] Failed unmarshalling action in category run for app %s: %s", secondAction.AppID, err)
					}

					parentWorkflow.Actions = []shuffle.Action{
						unmarshalledAction,
					}

					unparsedBody, err := handleStandaloneExecution(parentWorkflow)
					if err != nil {
						log.Printf("[ERROR] Failed running standalone execution: %s", err)
						return nil, err
					}

					// This is the format used in subflow executions, so we just pretend to be the same
					preparedResponse := shuffle.SubflowData{
						Success: 	 true,
						Result: 	 string(unparsedBody),
						ResultSet: true, 
					}

					apprunBody, err = json.Marshal(preparedResponse)
					if err != nil {
						log.Printf("[ERROR] Failed marshalling response for standalone execution: %s", err)
					}


				} else {
					// Sends back how many translations happened
					// -url is just for the app to parse it :(
					attemptString := "x-translation-attempt-url"
					if _, ok := resp.Header()[attemptString]; ok {
						resp.Header().Set(attemptString, fmt.Sprintf("%d", i+1))
					} else {
						resp.Header().Add(attemptString, fmt.Sprintf("%d", i+1))
					}

					//log.Printf("[DEBUG] Attempt preparedAction: %s", string(preparedAction))

					req, err := http.NewRequest(
						"POST",
						apprunUrl,
						bytes.NewBuffer(preparedAction),
					)

					if err != nil {
						log.Printf("[WARNING] Error in new request for execute generated app run (1): %s", err)
						respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed preparing new request. Contact support."}`))
						resp.WriteHeader(500)
						resp.Write(respBody)
						return respBody, err
					}

					for key, value := range request.Header {
						if len(value) == 0 {
							continue
						}

						req.Header.Add(key, value[0])
					}

					newresp, err := client.Do(req)
					if err != nil {
						log.Printf("[WARNING] Error running body for execute generated app run (2): %s", err)
						respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running generated app. Contact support."}`))
						resp.WriteHeader(500)
						resp.Write(respBody)
						return respBody, err
					}

					// Ensures frontend has something to debug if things go wrong
					for key, value := range newresp.Header {
						if strings.HasSuffix(strings.ToLower(key), "-url") {

							// Remove old ones with the same key
							if _, ok := resp.Header()[key]; ok {
								resp.Header().Set(key, value[0])
							} else {
								resp.Header().Add(key, value[0])
							}
						}
					}

					defer newresp.Body.Close()
					apprunBody, err = ioutil.ReadAll(newresp.Body)
					if err != nil {
						log.Printf("[WARNING] Failed reading body for execute generated app run (3): %s", err)
						respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed unmarshalling app response. Contact support."}`))
						resp.WriteHeader(500)
						resp.Write(respBody)
						return respBody, err
					}
				}
			}

			// Parse success struct. This JUST checks success.
			successStruct := shuffle.ResultChecker{}
			unmarshallErr := json.Unmarshal(apprunBody, &successStruct)
			if unmarshallErr != nil {
				log.Printf("[WARNING] Failed unmarshalling body for execute generated app run (4): %s", err)
			}

			httpOutput, marshalledBody, httpParseErr = shuffle.FindHttpBody(apprunBody)
			//log.Printf("\n\nGOT RESPONSE (%d): %s. STATUS: %d\n\n",  newresp.StatusCode, string(apprunBody), httpOutput.Status)
			if successStruct.Success == false && len(successStruct.Reason) > 0 && httpOutput.Status == 0 && strings.Contains(strings.ReplaceAll(string(apprunBody), " ", ""), `"success":false`) {
				log.Printf("[WARNING][AI] Failed running app %s (%s). Contact support. Reason: %s", selectedAction.Name, selectedAction.AppID, successStruct.Reason)

				resp.WriteHeader(400)
				resp.Write(apprunBody)
				return apprunBody, errors.New("Failed running app")
			}

			// Input value to get raw output instead of translated
			if value.SkipOutputTranslation {
				resp.WriteHeader(202)
				resp.Write(apprunBody)
				return apprunBody, nil
			}

			parsedTranslation := shuffle.SchemalessOutput{
				Success: false,
				Action:  value.Label,

				Status: httpOutput.Status,
				URL:    httpOutput.Url,
			}

			if !standalone { 
				for _, param := range secondAction.Parameters {
					if param.Name == "body" && len(param.Value) > 0 {
						translatedBodyString := "x-translated-body-url"
						if _, ok := resp.Header()[translatedBodyString]; ok {
							resp.Header().Set(translatedBodyString, param.Value)
						} else {
							resp.Header().Add(translatedBodyString, param.Value)
						}
					}

					if param.Name == "queries" && len(param.Value) > 0 {
						translatedQueryString := "x-translated-query-url"
						if _, ok := resp.Header()[translatedQueryString]; ok {
							resp.Header().Set(translatedQueryString, param.Value)
						} else {
							resp.Header().Add(translatedQueryString, param.Value)
						}
					}
				}
			}

			marshalledHttpOutput, marshalErr := json.Marshal(httpOutput)

			responseBodyString := "x-raw-response-url"
			if _, ok := resp.Header()[responseBodyString]; ok {
				resp.Header().Set(responseBodyString, string(marshalledHttpOutput))
			} else {
				resp.Header().Add(responseBodyString, string(marshalledHttpOutput))
			}

			if marshalErr == nil {

				if strings.HasPrefix(string(marshalledHttpOutput), "[") {
					outputArray := []interface{}{}
					err = json.Unmarshal(marshalledHttpOutput, &outputArray)
					if err != nil {
						log.Printf("[WARNING] Failed unmarshalling RAW list schemaless (3) output for label %s: %s", value.Label, err)

						parsedTranslation.RawResponse = string(marshalledHttpOutput)
					} else {
						parsedTranslation.RawResponse = outputArray
					}
				} else if strings.HasPrefix(string(marshalledHttpOutput), "{") {
					outputmap := map[string]interface{}{}
					err = json.Unmarshal(marshalledHttpOutput, &outputmap)
					if err != nil {
						log.Printf("[WARNING] Failed unmarshalling RAW {} schemaless (2) output for label %s: %s", value.Label, err)
					}

					parsedTranslation.RawResponse = outputmap
				} else {
					parsedTranslation.RawResponse = string(marshalledHttpOutput)
				}
			}

			if httpParseErr == nil && httpOutput.Status < 300 {
				if debug {
					log.Printf("[DEBUG] Found status from schemaless: %d. Saving the current fields as base. Attempts: %d, Request Time taken: %s", httpOutput.Status, i+1, time.Now().Sub(startTime))
				}

				parsedParameterMap := map[string]interface{}{}
				for _, param := range secondAction.Parameters {
					if param.Configuration {
						continue
					}

					if len(param.Value) == 0 {
						continue
					}

					if strings.Contains(param.Value, "&") && strings.Contains(param.Value, "=") {
						// Split by & and then by =
						parsedParameterMap[param.Name] = map[string]interface{}{}
						paramSplit := strings.Split(param.Value, "&")
						for _, paramValue := range paramSplit {
							paramValueSplit := strings.Split(paramValue, "=")
							if len(paramValueSplit) != 2 {
								continue
							}

							parsedParameterMap[param.Name].(map[string]interface{})[paramValueSplit[0]] = paramValueSplit[1]
						}
					} else {
						parsedParameterMap[param.Name] = param.Value
					}

					/*
					if param.Name != "body" {
						if debug { 
							log.Printf("[DEBUG] Skipping parameter save for '%s' (FOR NOW). Focusing ONLY on body field.", param.Name)
						}

						continue
					}
					*/

					if standalone { 
						shuffle.UploadParameterBase(context.Background(), user.ActiveOrg.Id, selectedApp.ID, secondAction.Name, param.Name, param.Value)
					} else {
						go shuffle.UploadParameterBase(context.Background(), user.ActiveOrg.Id, selectedApp.ID, secondAction.Name, param.Name, param.Value)
					}
				}

				if len(fieldHash) > 0 && fieldFileFound == false {
					inputFieldMap := map[string]interface{}{}
					for _, field := range value.Fields {
						inputFieldMap[field.Key] = field.Value
					}

					// Finds location of some data in another part of the data. This is to have a predefined location in subsequent requests
					// Allows us to map text -> field and not just field -> text (2-way)

					// Had to remove goroutine for standalone as it sometimes finished too fast and didn't have time to save
					if standalone { 
						StoreTranslationOutput(user, fieldHash, parsedParameterMap, inputFieldMap) 
					} else {
						go StoreTranslationOutput(user, fieldHash, parsedParameterMap, inputFieldMap) 
					}
				} else {
					if debug { 
						log.Printf("[DEBUG] Translation file already FOUND (%t) for hash: %#v. Look for file: '{root}/singul/%s'. NOT creating new one.", fieldFileFound, fieldHash, discoverFile)
					}
				}	

			} else {
				// Parses out data from the output
				// Reruns the app with the new parameters
				if debug {
					log.Printf("[DEBUG] Potential error handling: %#v", httpParseErr)
				}

				if strings.Contains(strings.ToLower(fmt.Sprintf("%s", httpParseErr)), "status: ") {
					if debug {
						log.Printf("[DEBUG] Found status code in schemaless error: %s", httpParseErr)
					}

					// Passing in:
					// secondAction: the action we've prepared 
					// apprunBody: the response body we got back from the app
					// additionalInfo: recursively filled in from this function 
					// inputQuery: the input we got initially
					outputString, outputAction, err, additionalInfo := shuffle.FindNextApiStep(
						secondAction, 
						apprunBody, 
						additionalInfo, 
						inputQuery, 
						originalAppname, 

						i+1,
					)
					if debug {
						log.Printf("[DEBUG]\n==== AUTOCORRECT ====\nOUTPUTSTRING: %s\nADDITIONALINFO: %s", outputString, additionalInfo)
					}

					// Rewriting before continuing
					// This makes the NEXT loop iterator run with the
					// output params from this one
					if err == nil && len(outputAction.Parameters) > 0 {
						secondAction.Name = outputAction.Name
						secondAction.Label = outputAction.Label
						secondAction.Parameters = outputAction.Parameters
						secondAction.InvalidParameters = outputAction.InvalidParameters
						preparedAction, err = json.Marshal(outputAction)
						if err != nil {
							resp.WriteHeader(202)
							resp.Write(apprunBody)
							return apprunBody, nil 
						}

						continue
					} else {

						if strings.Contains(fmt.Sprintf("%s", err), "missing_fields") && strings.Contains(fmt.Sprintf("%s", err), "success") {
							type missingFieldsStruct struct {
								Success 	  bool `json:"success"`
								MissingFields []string `json:"missing_fields"`
							}

							missingFields := missingFieldsStruct{}
							jsonerr := json.Unmarshal([]byte(err.Error()), &missingFields)
							if jsonerr != nil {
								log.Printf("[WARNING] Failed unmarshalling missing fields: %s", err)
							} else {
								//log.Printf("[DEBUG] Found missing fields: %s. Success: %#v", missingFields.MissingFields, missingFields.Success)
								if missingFields.Success == false {
									resp.WriteHeader(400)
									resp.Write(apprunBody)
									return apprunBody, err
								}
							}

							// Try to 
						} else {
							log.Printf("[ERROR] Problem in autocorrect (%d):\n%#v\nParams: %d", i, err, len(outputAction.Parameters))
						}

						if i < maxAttempts-1 {
							continue
						} else {
							resp.WriteHeader(400)
							resp.Write(apprunBody)
							return apprunBody, err
						}
					}
				} else {
					log.Printf("[ERROR] Failed parsing output in Singul: %s. This may mean that a status code was not found.", httpParseErr)
				}

				marshalledOutput, err := json.Marshal(parsedTranslation)
				if err != nil {
					log.Printf("[WARNING] Failed marshalling schemaless output for label %s: %s", value.Label, err)
					resp.WriteHeader(400)
					resp.Write(apprunBody)
					return apprunBody, err
				}

				resp.WriteHeader(400)
				resp.Write(marshalledOutput)
				return marshalledOutput, err
			}

			// Unmarshal responseBody back to secondAction
			// FIXME: add auth config to save translation files properly

			//streamUrl = fmt.Sprintf("%s&execution_id=%s&authorization=%s", streamUrl, request.URL.Query().Get("execution_id"), request.URL.Query().Get("authorization"))

			baseurlSplit := strings.Split(baseUrl, "/")
			if len(baseurlSplit) > 2 {
				// Only grab from https -> start of path
				baseUrl = strings.Join(baseurlSplit[0:3], "/")
			}

			// No shuffler.io config for standalone runs
			authConfig := fmt.Sprintf("%s,%s,%s,%s", baseUrl, authorization, orgId, optionalExecutionId)
			if standalone {
				authConfig = ""
			}

			outputmap := make(map[string]interface{})
			schemalessOutput, err := schemaless.Translate(ctx, value.Label, marshalledBody, authConfig)
			if err != nil {
				log.Printf("[ERROR] Failed translating schemaless output for label '%s': %s", value.Label, err)

				/*
					err = json.Unmarshal(marshalledBody, &outputmap)
					if err != nil {
						log.Printf("[WARNING] Failed unmarshalling in schemaless (1) output for label %s: %s", value.Label, err)

					} else {
						parsedTranslation.Output = outputmap
					}
				*/
			} else {
				parsedTranslation.Success = true
				//parsedTranslation.RawOutput = string(schemalessOutput)

				err = json.Unmarshal(schemalessOutput, &outputmap)
				if err != nil {

					// If it's an array and not a map, we should try to unmarshal it as an array
					if strings.HasPrefix(string(schemalessOutput), "[") {
						outputArray := []interface{}{}
						err = json.Unmarshal(schemalessOutput, &outputArray)
						if err != nil {
							log.Printf("[WARNING] Failed unmarshalling schemaless (3) output for label %s: %s", value.Label, err)
						}

						parsedTranslation.Output = outputArray
						parsedTranslation.RawResponse = nil
					} else {
						log.Printf("[WARNING] Failed unmarshalling schemaless (2) output for label %s: %s", value.Label, err)
					}
				} else {
					parsedTranslation.Output = outputmap
					//parsedTranslation.RawResponse = nil
				}

				// If it starts with list_ or serach_ only for now.
				// This is just to FORCE it to work and be locally testable
				foundLabelSplit := strings.Split(value.Label, "_")
				log.Printf("\n\n\n\nLABELSPLIT: %#v. FIX IN BACKEND!\n\n\n", foundLabelSplit)

				curApikey := shuffleApiKey
				curBackend := shuffleBackend
				curOrg := shuffleOrg

				// Can we do this within the context of the current request IF it was a request?
				// For cloud/self-hosted Shuffle:
				// 1. Run workflow with Singul action in Shuffle AI
				// with run_schemaless
				// 2. Shuffle AI app runs with execution_id=%s&authorization=%s to handle execution auth
				// 3. This means that the /api/v2/datastore?bulk=true API needs to support execution auth as well, as that's all we have access to. Since we have access to shuffle-shared, it could be possible 
				// to just set the data straight in the database as well,
				// but the problem then is that we do local validation


				// Conclusion: The /api/v1/datastore API needs to support execution auth

				if len(curApikey) == 0 || len(curBackend) == 0 || len(curOrg) == 0 {
					newAuth := request.URL.Query().Get("authorization")
					if len(newAuth) > 0 {
						curApikey = newAuth
					}

					// This is a hack to allow execution auth ((:
					newExecutionId := request.URL.Query().Get("execution_id")
					if len(newExecutionId) > 0 {
						curOrg = fmt.Sprintf("execution:%s", newExecutionId)
					}
				}

				if len(foundLabelSplit) > 1 && (strings.HasPrefix(value.Label, "list_") || strings.HasPrefix(value.Label, "search_")) && len(curApikey) > 0 && len(curOrg) > 0 {

					// Check if output is an array
					if debug {
						log.Printf("[DEBUG] Found list/search output for label %s.", value.Label)
					}

					// .Output = translated
					// .RawResponse = original (raw)
					if foundArray, ok := parsedTranslation.Output.([]interface{}); ok {
						log.Printf("[DEBUG] Found list/search output for label '%s'. Array Length: %d", value.Label, len(foundArray))

						actualLabel := strings.ToLower(strings.Join(foundLabelSplit[1:], "_"))
						allEntries := []shuffle.CacheKeyData{}


						// Goroutine BUT wait on the end
						for cnt, item := range foundArray {
							// Check if uid, uuid, id or similar is a valid key
							if _, ok := item.(map[string]interface{}); !ok {
								log.Printf("[WARNING] Item in list output for label %s is not a map: %#v", value.Label, item)
								continue
							}

							generatedItem := item.(map[string]interface{})

							idKeys := []string{"id", "uid", "uuid", "identifier", "key"}

							foundIdentifier := ""
							for _, idKey := range idKeys {
								// Check if lower case exists
								for key, mapValue := range generatedItem {
									if strings.ToLower(key) != idKey {
										continue
									}

									// Found the key, set it as the actual label
									if val, ok := mapValue.(string); ok {
										foundIdentifier = val
										break
									} else {
										log.Printf("[ERROR] Failed finding ID key in item for label %s: %s", value.Label, mapValue)
									}
								}

								if foundIdentifier != "" {
									break
								}
							}

							// hardcoded override for product name
							toolKeys := []string{"product", "tool", "service", "application", "app"}
							if len(secondAction.AppName) > 0 { 
								for _, toolKey := range toolKeys {
									for key, mapValue := range generatedItem {
										if strings.ToLower(key) != toolKey {
											continue
										}

										// Found the key, set it to the appname
										if _, ok := mapValue.(string); ok {
											generatedItem[key] = strings.ToLower(secondAction.AppName)
											break
										}

									}
								}
							}

							if len(foundIdentifier) == 0 {
								log.Printf("[ERROR] Failed finding ID key in item for label %s: %#v", value.Label, item)
								break
							}

							// Check if we have already uploaded it recently based on identifier
							cacheKey := fmt.Sprintf("singul_%s_%s", orgId, foundIdentifier)
							_, err := shuffle.GetCache(ctx, cacheKey)
							if err == nil {
								if debug { 
									log.Printf("[DEBUG] Found item in cache for label %s in org %s", foundIdentifier, orgId)
								}

								// We already have it, skip
								continue
							}

							shuffle.SetCache(ctx, cacheKey, []byte("true"), 60*60*24*3) // 3 days

							marshalledBody, err := json.Marshal(item)
							if err != nil {
								log.Printf("[ERROR] Failed marshalling item in schemaless output for label %s: %s", value.Label, err)
								continue
							}

							datastoreEntry := shuffle.CacheKeyData{
								Key: foundIdentifier,
								Value: string(marshalledBody),
								Category: actualLabel,
							}
						
							allEntries = append(allEntries, datastoreEntry)

							if cnt > 100 {
								break
							}
						}

						// v0.1: Send /api/v1/orgs/{org_id}/set_cache requrest
						// v0.2: Send /api/v1/orgs/{org_id}/datastore/upload request

						// Bulk request instead (shuffle 2.1.0 optimisation)
						err = sendDatastoreUploadRequest(ctx, allEntries, curApikey, curOrg, curBackend)
						if err != nil {
							log.Printf("[WARNING] Failed sending datastore request (3) for item in schemaless output for label %s: %s", value.Label, err)
						} else {
							log.Printf("[INFO] Successfully stored %d items in schemaless/singul output for label %s in org %s", len(allEntries), value.Label, orgId)
						}

					}

					log.Printf("\n\n\n")
				}
			}

			// Check if length of output exists. If it does, remove raw output
			//if len(parsedTranslation.Output) > 0 {
			//	parsedTranslation.RawOutput = ""
			//}

			marshalledOutput, err := json.Marshal(parsedTranslation)
			if err != nil {
				log.Printf("[WARNING] Failed marshalling schemaless output for label %s: %s", value.Label, err)
				resp.WriteHeader(202)
				resp.Write(schemalessOutput)
				return schemalessOutput, err
			}

			resp.WriteHeader(200)
			resp.Write(marshalledOutput)
			return marshalledOutput, nil

		}

		if debug { 
			log.Printf("\n\n\n[DEBUG] Done in autocorrect loop\n\n\n")
		}
	}

	parentWorkflow.Start = secondAction.ID
	parentWorkflow.Actions = append(parentWorkflow.Actions, secondAction)

	// Used to be where we stopped
	//resp.Write(responseBody)
	//resp.WriteHeader(200)
	//return
	/*
	*
	* Below is all the old stuff before we started doing atomic functions
	* the goal is now to make this a lot more dynamic and not have to
	* know everything beforehand
	*
	 */

	// Add params and such of course

	// Set workflow in cache only?
	// That way it can be loaded during execution

	if startNode.ID != "" {
		// Check if the workflow contains a trigger
		// If it doesn't, add one. Schedule or Webhook.
		// Default: schedule. Webhook is an option added

		triggersFound := 0
		for _, trigger := range parentWorkflow.Triggers {
			if trigger.Name != "Shuffle Workflow" && trigger.Name != "User Input" {
				triggersFound++
			}
		}

		if triggersFound == 0 {

			decidedTrigger := shuffle.Trigger{
				Name:        "Schedule",
				Status:      "uninitialized",
				TriggerType: "SCHEDULE",
				IsValid:     true,
				Label:       "Scheduler",
			}

			if strings.Contains(strings.ToLower(strings.Join(selectedApp.ReferenceInfo.Triggers, ",")), "webhook") {
				// Add a schedule trigger
				decidedTrigger = shuffle.Trigger{
					Name:        "Webhook",
					Status:      "uninitialized",
					TriggerType: "WEBHOOK",
					IsValid:     true,
					Label:       "Webhook",
				}
			} else {
				// Check if it's CRUD (Read)
				// If it's List + schedule, we should add some kind of filter after 1st node

				// Check if startnode has "list" or "search" in it
				if strings.Contains(strings.ToLower(startNode.Name), "list") || strings.Contains(strings.ToLower(startNode.Name), "search") {
					log.Printf("[DEBUG] Found list/search in startnode name, adding filter after trigger? Can this be automatic?")
				}

			}

			decidedTrigger.ID = uuid.NewV4().String()
			decidedTrigger.Position = shuffle.Position{
				X: startNode.Position.X - 200,
				Y: startNode.Position.Y,
			}
			decidedTrigger.AppAssociation = shuffle.WorkflowApp{
				Name:       selectedApp.Name,
				AppVersion: selectedApp.AppVersion,
				ID:         selectedApp.ID,
				LargeImage: selectedApp.LargeImage,
			}

			parentWorkflow.Triggers = append(parentWorkflow.Triggers, decidedTrigger)
			// Add a branch from trigger to startnode
			parentWorkflow.Branches = append(parentWorkflow.Branches, shuffle.Branch{
				SourceID:      decidedTrigger.ID,
				DestinationID: startNode.ID,
				ID:            uuid.NewV4().String(),
			})

			log.Printf("[DEBUG] Added trigger to the generated workflow")
		}
	}

	if !standalone { 
		err = shuffle.SetWorkflow(ctx, parentWorkflow, parentWorkflow.ID)
		if err != nil {
			log.Printf("[WARNING] Failed setting workflow during category run: %s", err)
		}
	} else {
		return handleStandaloneExecution(parentWorkflow)
	}

	log.Printf("[DEBUG] Done preparing workflow '%s' (%s) to be ran for category action %s", parentWorkflow.Name, parentWorkflow.ID, selectedAction.Name)

	if value.DryRun {
		log.Printf("\n\n[DEBUG] Returning before execution because dry run is set\n\n")
		if len(startNode.ID) > 0 {
			log.Printf("[DEBUG] GOT STARTNODE: %s (%s)", startNode.Name, startNode.ID)
			// Find the action and set it as the new startnode
			// This works as the execution is already done
			for actionIndex, action := range parentWorkflow.Actions {
				if action.ID == startNode.ID {
					parentWorkflow.Start = startNode.ID
					parentWorkflow.Actions[actionIndex].IsStartNode = true
				} else {
					parentWorkflow.Actions[actionIndex].IsStartNode = false
				}
			}

			// Save the workflow
			err = shuffle.SetWorkflow(ctx, parentWorkflow, parentWorkflow.ID)
			if err != nil {
				log.Printf("[WARNING] Failed saving workflow in run category action: %s", err)
			}
		}

		successOutput := []byte(fmt.Sprintf(`{"success": true, "dry_run": true, "workflow_id": "%s", "reason": "Steps built, but workflow not executed"}`, parentWorkflow.ID))
		resp.Write(successOutput)
		resp.WriteHeader(200)

		return successOutput, nil
	}

	// FIXME: Make dynamic? AKA input itself is what controls the workflow?
	// E.g. for the "body", instead of having to know it's "body" we just have to know it's "input" and dynamically fill in based on execution args
	executionArgument := ""
	execData := shuffle.ExecutionStruct{
		Start:             startId,
		ExecutionSource:   "ShuffleGPT",
		ExecutionArgument: executionArgument,
	}

	newExecBody, err := json.Marshal(execData)
	if err != nil {
		log.Printf("[DEBUG] Failed ShuffleGPT data formatting: %s", err)
	}

	// Starting execution
	workflowRunUrl := fmt.Sprintf("%s/api/v1/workflows/%s/execute", baseUrl, parentWorkflow.ID)
	req, err := http.NewRequest(
		"POST",
		workflowRunUrl,
		bytes.NewBuffer(newExecBody),
	)
	if err != nil {
		log.Printf("[WARNING] Error in new request for execute generated workflow: %s", err)
		respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed preparing new request. Contact support."}`))
		resp.WriteHeader(500)
		resp.Write(respBody)
		return respBody, err
	}

	req.Header.Add("Authorization", request.Header.Get("Authorization"))
	newresp, err := client.Do(req)
	if err != nil {
		log.Printf("[WARNING] Error running body for execute generated workflow: %s", err)
		respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running generated app. Contact support."}`))
		resp.WriteHeader(500)
		resp.Write(respBody)
		return respBody, err
	}

	defer newresp.Body.Close()
	executionBody, err := ioutil.ReadAll(newresp.Body)
	if err != nil {
		log.Printf("[WARNING] Failed reading body for execute generated workflow: %s", err)
		respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed unmarshalling app response. Contact support."}`))
		resp.WriteHeader(500)
		resp.Write(respBody)
		return respBody, err
	}

	workflowExecution := shuffle.WorkflowExecution{}
	err = json.Unmarshal(executionBody, &workflowExecution)
	if err != nil {
		log.Printf("[WARNING] Failed unmarshalling body for execute generated workflow: %s %+v", err, string(executionBody))
	}

	if len(workflowExecution.ExecutionId) == 0 {
		log.Printf("[ERROR] Failed running app %s (%s) in org %s (%s). Raw: %s", selectedApp.Name, selectedApp.ID, org.Name, org.Id, string(executionBody))
		respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running the app. Contact support@shuffler.io if this persists."}`))
		resp.WriteHeader(500)
		resp.Write(respBody)
		return respBody, errors.New("Failed running app")
	}

	returnBody := shuffle.HandleRetValidation(ctx, workflowExecution, len(parentWorkflow.Actions))
	selectedApp.LargeImage = ""
	selectedApp.Actions = []shuffle.WorkflowAppAction{}
	structuredFeedback := shuffle.StructuredCategoryAction{
		Success:        true,
		WorkflowId:     parentWorkflow.ID,
		ExecutionId:    workflowExecution.ExecutionId,
		Action:         "done",
		Category:       discoveredCategory,
		Reason:         "Analyze Result for details",
		ApiDebuggerUrl: fmt.Sprintf("https://shuffler.io/apis/%s", selectedApp.ID),
		Result:         returnBody.Result,
	}

	jsonParsed, err := json.Marshal(structuredFeedback)
	if err != nil {
		log.Printf("[ERROR] Failed marshalling structured feedback: %s", err)
		respBody = []byte(`{"success": false, "reason": "Failed marshalling structured feedback (END)"}`)	
		resp.WriteHeader(500)
		resp.Write(respBody)
		return respBody, err
	}

	if len(startNode.ID) > 0 {
		log.Printf("[DEBUG] GOT STARTNODE: %s (%s)", startNode.Name, startNode.ID)
		// Find the action and set it as the new startnode
		// This works as the execution is already done
		for actionIndex, action := range parentWorkflow.Actions {
			if action.ID == startNode.ID {
				parentWorkflow.Start = startNode.ID
				parentWorkflow.Actions[actionIndex].IsStartNode = true
			} else {
				parentWorkflow.Actions[actionIndex].IsStartNode = false
			}
		}

		// Save the workflow
		err = shuffle.SetWorkflow(ctx, parentWorkflow, parentWorkflow.ID)
		if err != nil {
			log.Printf("[WARNING] Failed saving workflow in run category action: %s", err)
		}
	}

	resp.WriteHeader(200)
	resp.Write(jsonParsed)
	return jsonParsed, nil
}

func sendDatastoreUploadRequest(ctx context.Context, datastoreEntry []shuffle.CacheKeyData, apikey, orgId, backendUrl string) error {


	// Send a request to /api/v1/orgs/{org_id}/set_cache
	if !strings.HasPrefix(backendUrl, "http") {
		backendUrl = os.Getenv("BASE_URL")

		if len(os.Getenv("SHUFFLE_CLOUDRUN_URL")) > 0 {
			backendUrl = os.Getenv("SHUFFLE_CLOUDRUN_URL")
		}

		if len(backendUrl) == 0 {
			return errors.New("Backend URL is not set. Please set SHUFFLE_CLOUDRUN_URL or BASE_URL.")
		}
	}

	url := fmt.Sprintf("%s/api/v2/datastore?bulk=true", backendUrl)

	reqBody, err := json.Marshal(datastoreEntry)
	if err != nil {
		log.Printf("[ERROR] Failed marshalling cache item before send: %s", err)
		return err
	}

	// Execution auth workaround
	if strings.HasPrefix(orgId, "execution:") {
		executionId := strings.TrimPrefix(orgId, "execution:")
		url += fmt.Sprintf("%s&execution_id=%s&authorization=%s", url, executionId, apikey)

		orgId = ""
		apikey = ""
	}

	// May need a fixed client here
	client := &http.Client{}
	req, err := http.NewRequest(
		"POST",
		url,
		bytes.NewBuffer(reqBody),
	)

	if len(orgId) > 0 {
		req.Header.Add("Org-Id", orgId)
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", apikey))

	res, err := client.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if res.StatusCode != 200 {
		log.Printf("[ERROR] Failed sending datastore request: %s. Status code: %d. Body: %s", err, res.StatusCode, string(body))
		return errors.New(fmt.Sprintf("Failed sending datastore request (2): %s. Status code: %d. Body: %s", err, res.StatusCode, string(body)))
	}

	//log.Printf("[DEBUG] Successfully sent datastore request for %d items
	return nil
}


// Checks if all input fields are in the action or not.
// This just uses the value, and looks for what may be missing
func validatePreparedActionHasFields(preparedAction []byte, fields []shuffle.Valuereplace) map[string]string {
	missingFieldsInBody := map[string]string{}
	if os.Getenv("SKIP_VALIDATION") == "true" {
		if debug {
			log.Printf("[DEBUG] Skipping validation of prepared action. This may cause actions to not have all the relevant fields. Please send different input fields for this action.")
		}

		return missingFieldsInBody
	}

	unmarshalledAction := shuffle.Action{}
	err := json.Unmarshal(preparedAction, &unmarshalledAction)
	if err != nil {
		log.Printf("[ERROR] Failed unmarshalling action in VALIDATE PreparedAction %s: %s", unmarshalledAction.AppID, err)
	}

	for _, field := range fields {
		found := false
		for _, param := range unmarshalledAction.Parameters {
			if strings.Contains(param.Value, field.Value) {
				found = true
				break
			}
		}

		if !found {
			missingFieldsInBody[field.Key] = field.Value
		}
	}


	// Deleting all fields IF anything is missing.

	if len(missingFieldsInBody) > 0 { 
		if debug {
			log.Printf("[DEBUG] Removing translation files as they seem to be missing content. This WILL be overridden if the api is successful and contains all relevant keys")
		}

		for _, param := range unmarshalledAction.Parameters {
			if param.Configuration {
				continue
			}

			fileId := fmt.Sprintf("file_parameter_-%s-%s-%s.json", strings.ToLower(unmarshalledAction.AppID), strings.Replace(strings.ToLower(unmarshalledAction.Name), " ", "_", -1), strings.ToLower(param.Name))

			category := "app_defaults"
			if standalone {
				fileId = fmt.Sprintf("%s/%s", category, fileId)
			}

			go shuffle.DeleteFileSingul(context.Background(), fileId)
		}
	}

	return missingFieldsInBody

}


func StoreTranslationOutput(user shuffle.User, fieldHash string, parsedParameterMap map[string]interface{}, inputFieldMap map[string]interface{}) {
	reversed, err := schemaless.ReverseTranslate(parsedParameterMap, inputFieldMap)
	if err != nil {
		log.Printf("[ERROR] Problem with reversing: %s", err)
	} else {
		log.Printf("[DEBUG] Raw reverse: %s", reversed)

		finishedFields := 0
		mappedFields := map[string]interface{}{}
		err = json.Unmarshal([]byte(reversed), &mappedFields)
		if err == nil {
			for _, value := range mappedFields {
				if _, ok := value.(string); ok && len(value.(string)) > 0 {
					finishedFields++
				} else {
					log.Printf("[DEBUG] Found non-string value: %#v", value)
				}
			}
		}

		if debug { 
			log.Printf("[DEBUG] Reversed fields (%d): %s", finishedFields, reversed)
		}

		if finishedFields == 0 {
		} else {

			timeNow := time.Now().Unix()

			fileId := fmt.Sprintf("file_%s", fieldHash)
			encryptionKey := fmt.Sprintf("%s_%s", user.ActiveOrg.Id, fileId)
			folderPath := fmt.Sprintf("%s/%s/%s", basepath, user.ActiveOrg.Id, "global")
			downloadPath := fmt.Sprintf("%s/%s", folderPath, fileId)
			file := &shuffle.File{
				Id:           fileId,
				CreatedAt:    timeNow,
				UpdatedAt:    timeNow,
				Description:  "",
				Status:       "active",
				Filename:     fmt.Sprintf("%s.json", fieldHash),
				OrgId:        user.ActiveOrg.Id,
				WorkflowId:   "global",
				DownloadPath: downloadPath,
				Subflows:     []string{},
				StorageArea:  "local",
				Namespace:    "translation_output",
				Tags: []string{
					"autocomplete",
				},
			}

			returnedId, err := shuffle.UploadFileSingul(context.Background(), file, encryptionKey, []byte(reversed))
			if err != nil {
				log.Printf("[ERROR] Problem uploading file: %s", err)
			} else {
				log.Printf("[DEBUG] Uploaded file with ID: %s", returnedId)
			}
		}
	}
}


func GetAppOpenapi(appname string) (openapi3.Swagger, error) {
	swaggerOutput := openapi3.Swagger{}
	appPath := fmt.Sprintf("%s/apps/%s.json", basepath, appname)

	if debug {
		log.Printf("[DEBUG] Looking for app '%s' in path '%s'", appname, appPath)
	}

	// Check if the app exists
	// Read the file
	reader, err := os.Open(appPath)
	if err != nil {
		log.Printf("[WARNING] Failed opening app file: %s", err)
		return swaggerOutput, err
	}

	defer reader.Close()
	responseBody, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Printf("[WARNING] Failed reading app file: %s", err)
		return swaggerOutput, err
	}

	newApp := shuffle.AppParser{}
	err = json.Unmarshal(responseBody, &newApp)
	if err != nil {
		log.Printf("[WARNING] Failed unmarshalling body for singul app: %s %+v", err, string(responseBody))
		return swaggerOutput, err
	}

	if !newApp.Success {
		return swaggerOutput, errors.New("Failed getting app details from backend. Please try again. Appnames may be case sensitive.")
	}

	if len(newApp.OpenAPI) == 0 {
		return swaggerOutput, errors.New("Failed finding app for this ID")
	}

	openapiWrapperParser := shuffle.ParsedOpenApi{}
	err = json.Unmarshal(newApp.OpenAPI, &openapiWrapperParser)
	if err != nil {
		log.Printf("[WARNING] Failed unmarshalling app: %s", err)
		return swaggerOutput, err
	}

	if len(openapiWrapperParser.Body) == 0 {
		return swaggerOutput, errors.New("Failed finding app for this ID")
	}

	// Unmarshal the newApp.App into workflowApp
	//err = json.Unmarshal(newApp.OpenAPI, &parsedApp)
	swagger, err := openapi3.NewSwaggerLoader().LoadSwaggerFromData([]byte(openapiWrapperParser.Body))
	if err != nil {
		log.Printf("[WARNING] Failed unmarshalling app: %s", err)
		return *swagger, err
	}

	return *swagger, nil
}

func Setupvenv(appscriptFolder string) (string, error) {
	pythonEnvPath := fmt.Sprintf("%s/venv", appscriptFolder)

	if _, err := os.Stat(pythonEnvPath); os.IsNotExist(err) {
		log.Printf("[DEBUG] Python virtual environment does not exist. Creating it at: %s", pythonEnvPath)
		cmd := exec.Command("python3", "-m", "venv", pythonEnvPath)
		// cmd.Dir = appscriptFolder

		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[WARNING] Failed getting stdout pipe: %s", err)
			return "", err
		}

		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			log.Printf("[WARNING] Failed getting stderr pipe: %s", err)
			return "", err
		}

		var stdoutBuf, stderrBuf bytes.Buffer
		if err := cmd.Start(); err != nil {
			log.Printf("[WARNING] Failed starting command (1): %s", err)
			return "", err
		}

		// Copy output to buffers
		go io.Copy(&stdoutBuf, stdoutPipe)
		go io.Copy(&stderrBuf, stderrPipe)

		// Wait for the command to finish
		if err := cmd.Wait(); err != nil {
			log.Printf("[WARNING] Failed waiting for command (2): %s", err)
			return "", err
		}

		// Access outputs as strings
		_ = stdoutBuf.String()
		_ = stderrBuf.String()

		// log.Printf("[DEBUG] Command stdout: %s", stdoutStr)
		// log.Printf("[DEBUG] Command stderr: %s", stderrStr)

		// verify that it created venv
		if _, err := os.Stat(pythonEnvPath); os.IsNotExist(err) {
			log.Printf("[WARNING] Python virtual environment was not created successfully at: %s", pythonEnvPath)
			return "", errors.New("Failed creating python virtual environment")
		}

		log.Printf("[DEBUG] Successfully created python virtual environment at: %s", pythonEnvPath)


		absolutePath, err := filepath.Abs(pythonEnvPath)
		if err != nil {
			fmt.Printf("Error getting absolute path: %v\n", err)
			return "", err
		}

		log.Printf("[DEBUG] Make sure to give the python virtual environment executable permissions\nWith: chmod +x %s/bin/python3", absolutePath)

		// wait for user to confirm from input
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Printf("[DEBUG] Press Enter to confirm after giving the python virtual environment executable permissions (chmod +x %s/bin/python3): ", absolutePath)
		scanner.Scan()

	}

	return pythonEnvPath, nil
}

func SetupRequirements(requirementsPath string) error {
	pythonEnvPath, err := Setupvenv(fmt.Sprintf("%s/scripts", basepath))
	if err != nil {
		log.Printf("[WARNING] Failed setting up python virtual environment: %s", err)
		return err
	}

	requirements := shuffle.GetAppRequirements()
	err = ioutil.WriteFile(requirementsPath, []byte(requirements), 0644)
	if err != nil {
		log.Printf("[WARNING] Failed writing requirements file: %s", err)
	} else {
		pythonPath := fmt.Sprintf("%s/bin/python3", pythonEnvPath)

		log.Printf("[DEBUG] Successfully wrote requirements file: %s", requirementsPath)
		cmd := exec.Command(pythonPath, "-m", "pip", "install", "-r", requirementsPath)

		// log.Printf("[DEBUG] Running command: %s with dir %s", cmd.String(), cmd.Dir)

		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[WARNING] Failed getting stdout pipe: %s", err)
			return err
		}

		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			log.Printf("[WARNING] Failed getting stderr pipe: %s", err)
			return err
		}

		var stdoutBuf, stderrBuf bytes.Buffer
		if err := cmd.Start(); err != nil {
			log.Printf("[WARNING] Failed starting command (2): %s", err)
			return err
		}

		// Copy output to buffers
		go io.Copy(&stdoutBuf, stdoutPipe)
		go io.Copy(&stderrBuf, stderrPipe)

		// Wait for the command to finish
		if err := cmd.Wait(); err != nil {
			log.Printf("[WARNING] Failed waiting for command (3): %s", err)
			return err
		}

		_ = stdoutBuf.String()
		_ = stderrBuf.String()

		// log.Printf("[DEBUG] Command stdout: %s", stdoutStr)
		// log.Printf("[DEBUG] Command stderr: %s", stderrStr)

		if err != nil {
			log.Printf("[WARNING] Failed installing requirements: %s", err)
		} else {
			log.Printf("[DEBUG] Successfully installed requirements for app script: %s", requirementsPath)
		}
	}

	return nil
}

func LocalizeAppscript(appname string) (string, error) {
	appscriptFolder := fmt.Sprintf("%s/scripts", basepath)
	scriptPath := fmt.Sprintf("%s/%s.py", appscriptFolder, appname)

	// Create the folders
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		log.Printf("[WARNING] Script path does not exist: %s. Trying to generate.", scriptPath)
		// Read the appPath -> Unmarshal Openapi into inputSwagger
		inputSwagger, err := GetAppOpenapi(appname)
		if err != nil {
			return "", err
		}

		newmd5 := fmt.Sprintf("%s-%s", appname, "1.1.0")
		_, _, pythonFunctions, err := shuffle.GenerateYaml(&inputSwagger, newmd5) 
		if err != nil {
			log.Printf("[WARNING] Failed generating app script: %s", err)
			return "", err
		}

		//_ = openapiSpec
		//_ = app

		pythoncode := fmt.Sprintf(shuffle.GetBasePython(), appname, "1.1.0", appname, strings.Join(pythonFunctions, "\n"), appname)

		// Write pythoncode to scriptPath
		err = os.MkdirAll(appscriptFolder, os.ModePerm)
		if err != nil {
			return "", err
		}

		err = ioutil.WriteFile(scriptPath, []byte(pythoncode), 0644)
		if err != nil {
			log.Printf("[WARNING] Failed writing script file: %s", err)
			return "", err
		}

		requirementsPath := fmt.Sprintf("%s/requirements.txt", appscriptFolder)

		if _, err := os.Stat(requirementsPath); os.IsNotExist(err) {
			err = SetupRequirements(requirementsPath)
			if err != nil {
				log.Printf("[WARNING] Failed setting up requirements: %s", err)
				return "", err
			}
		}

		return pythoncode, nil
	} 

	// Check if the script exists
	file, err := os.Open(scriptPath)
	if err != nil {
		log.Printf("[WARNING] Failed opening script file: %s", err)
		return "", err
	}

	defer file.Close()
	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Printf("[WARNING] Failed reading script file: %s", err)
		return "", err
	}

	// Check if the script is empty
	if len(data) == 0 {
		log.Printf("[WARNING] Script file is empty: %s", scriptPath)
		return "", errors.New("Script file is empty")
	}

	return string(data), nil
}

func handleStandaloneExecution(workflow shuffle.Workflow) ([]byte, error) {
	if len(os.Getenv("SHUFFLE_APP_SDK_TIMEOUT")) == 0 {
		appTimeout := "30"
		os.Setenv("SHUFFLE_APP_SDK_TIMEOUT", appTimeout)
		log.Printf("Set App timeout to %s seconds", appTimeout)
	}

	returnBody := []byte(fmt.Sprintf(`{"success": false, "reason": "No action taken"}`))
	if len(workflow.Actions) != 1 {
		log.Printf("[DEBUG] EXECUTION ACTIONS: %d. CHoosing the LAST node.", len(workflow.Actions))
		for _, action := range workflow.Actions {
			log.Printf("[DEBUG] ACTION - Name: %s, Label: %s, AppID: %s", action.Name, action.Label, action.AppID)
		}

		workflow.Actions = []shuffle.Action{
			workflow.Actions[len(workflow.Actions)-1],
		}

		//returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Only one action can be executed in standalone mode"}`))
		//return returnBody, errors.New(fmt.Sprintf("Only one action can be executed in standalone mode. Found: %d", len(workflow.Actions)))
	}

	action := workflow.Actions[0]
	allAuths, err := GetLocalAuth() 
	if err != nil {
		returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed getting local auths"}`))
		return returnBody, err
	}

	sampleExec := shuffle.WorkflowExecution{}
	action, _ = shuffle.GetAuthentication(context.Background(), sampleExec, action, allAuths) 

	// Look for the app script for the app
	appName := strings.TrimSpace(strings.ReplaceAll(strings.ToLower(action.AppName), " ", "_"))
	appscript, err := LocalizeAppscript(appName)
	if err != nil || len(appscript) == 0 {
		log.Printf("[WARNING] Failed localizing app script: %s", err)
		returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed localizing app script"}`))
		return returnBody, err
	}

	// Run the script
	// Command:
	appscriptFolder := fmt.Sprintf("%s/scripts", basepath)
	scriptPath := fmt.Sprintf("%s/%s.py", appscriptFolder, appName)
	// python3 scriptPath --action actionName --params params

	pythonCommandSplit := []string{scriptPath, "--standalone", "--action=" + action.Name}
	for _, param := range action.Parameters {
		if param.Required || len(param.Value) > 0 { 

			// Will NEED quotes in the future, but the sdk doesn't support it properly...
			//newCommand := fmt.Sprintf(`--%s='%s'`, param.Name, param.Value)
			newCommand := fmt.Sprintf(`--%s=%s`, param.Name, param.Value)
			pythonCommandSplit = append(pythonCommandSplit, newCommand)
		}
	}

	pythonEnvPath, err := Setupvenv(appscriptFolder)	
	if err != nil {
		log.Printf("[WARNING] Failed setting up python virtual environment: %s", err)
		return returnBody, err
	}

	pythonPath := fmt.Sprintf("%s/bin/python3", pythonEnvPath)

	// Run the command
	cmd := exec.Command(pythonPath, pythonCommandSplit...)

	//cmd.Dir = appscriptFolder
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[WARNING] Failed getting stdout pipe: %s", err)
		returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed getting stdout pipe"}`))
		return returnBody, err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[WARNING] Failed getting stderr pipe: %s", err)
		returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed getting stderr pipe"}`))
		return returnBody, err
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := cmd.Start(); err != nil {
		log.Printf("[WARNING] Failed starting command (3): %s", err)
		returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed starting command"}`))
		return returnBody, err
	}

	// Copy output to buffers
	io.Copy(&stdoutBuf, stdoutPipe)
	io.Copy(&stderrBuf, stderrPipe)

	if debug { 
		log.Printf("[DEBUG] Running command: python3 %s", strings.Join(pythonCommandSplit, " "))
	}

	// Wait for the command to finish
	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	if err := cmd.Wait(); err != nil {
		log.Printf("[ERROR] Failed waiting for command (1): %s", err)

		if len(stderrStr) > 0 {
			log.Printf("[ERROR] Command stderr (1): %s", stderrStr)
			returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running command", "stderr": "%s"}`, stderrStr))
		} else {
			if len(stdoutStr) > 0 {
				log.Printf("[WARNING] Command stdout (1): %s", stdoutStr)
				returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running command", "stdout": "%s"}`, stdoutStr))
			} else {
				returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running command"}`))
			}
		}

		return returnBody, err
	}

	if debug { 
		log.Printf("STDERR1: %s", stderrStr)
	}

	record := false
	relevantLines := []string{}
	for _, line := range strings.Split(stdoutStr, "\n") {
		if record {
			relevantLines = append(relevantLines, line)
		}

		if strings.Contains(line, "====") {
			record = true
		}
	}

	/*
	if len(stderrStr) > 0 {
		returnBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running command", "stderr": "%s"}`, stderrStr))
		log.Printf("[WARNING] Failed running command: %s", stderrStr)
		return returnBody, errors.New("Failed running command")
	}
	*/

	output := strings.Join(relevantLines, "\n")
	return []byte(output), nil
}

func GetOrgspecificParameters(ctx context.Context, org shuffle.Org, action shuffle.WorkflowAppAction) shuffle.WorkflowAppAction {
	//log.Printf("\n\n[DEBUG] LOADING ORG SPECIFIC PARAMETERS\n\n")
	for paramIndex, param := range action.Parameters {
		if param.Configuration {
			continue
		}

		if len(param.Options) > 0 {
			continue
		}

		fileId := fmt.Sprintf("file_%s-%s-%s-%s.json", org.Id, strings.ToLower(action.AppID), strings.Replace(strings.ToLower(action.Name), " ", "_", -1), strings.ToLower(param.Name))

		// Ensures we load from the correct folder
		if standalone {
			fileId = fmt.Sprintf("file_parameter_%s-%s-%s-%s.json", org.Id, strings.ToLower(action.AppID), strings.Replace(strings.ToLower(action.Name), " ", "_", -1), strings.ToLower(param.Name))
			fileId = fmt.Sprintf("app_defaults/%s", fileId)
		}

		file, err := shuffle.GetFileSingul(ctx, fileId)
		if err != nil || file.Status != "active" {
			/*
			if debug { 
				log.Printf("[WARNING] Parameter file %s%s NOT found or not active. Status: %#v. Err: %s", shuffle.GetSingulStandaloneFilepath(), fileId, file.Status, err)
			}
			*/

			continue
		}

		if file.OrgId != org.Id {
			file.OrgId = org.Id
		}

		// make a fake resp to get the content
		//func GetFileContent(ctx context.Context, file *File, resp http.ResponseWriter) ([]byte, error) {
		content, err := shuffle.GetFileContentSingul(ctx, file, nil)
		if err != nil {
			continue
		}

		if len(content) < 5 {
			continue
		}

		//log.Printf("[INFO] Found content for file %s for action %s in app %s. Should set param.", fileId, action.Name, action.AppName)
		//action.Parameters[paramIndex].Example = string(content)
		action.Parameters[paramIndex].Value = string(content)
	}

	return action
}

func GetActionFromLabel(ctx context.Context, app shuffle.WorkflowApp, label string, fixLabels bool, fields []shuffle.Valuereplace, count int) (shuffle.WorkflowAppAction, shuffle.AppCategory, []string) {
	availableLabels := []string{}
	selectedCategory := shuffle.AppCategory{}
	selectedAction := shuffle.WorkflowAppAction{}

	if len(app.ID) == 0 || len(app.Actions) == 0 {
		log.Printf("[WARNING] No actions in app %s (%s) for label '%s'", app.Name, app.ID, label)
		return selectedAction, selectedCategory, availableLabels
	}

	// Reload the app to be the proper one with updated actions instead
	// of random cache issues
	if !standalone {
		newApp, err := shuffle.GetApp(ctx, app.ID, shuffle.User{}, false)
		if err != nil {
			log.Printf("[WARNING] Failed getting app in category action: %s", err)
		} else {
			app = *newApp
		}
	}

	categories := shuffle.GetAllAppCategories()
	lowercaseLabel := strings.ReplaceAll(strings.ToLower(label), " ", "_")
	exactMatch := false
	for _, action := range app.Actions {
		if len(action.CategoryLabel) == 0 {
			//log.Printf("%s: %#v\n", action.Name, action.CategoryLabel)
			continue
		}

		//log.Printf("FOUND LABELS: %s -> %#v\n", action.Name, action.CategoryLabel)

		for _, label := range action.CategoryLabel {
			newLabel := strings.ReplaceAll(strings.ToLower(label), " ", "_")
			if newLabel == "no_label" {
				continue
			}

			// To ensure we have both normal + parsed label
			availableLabels = append(availableLabels, newLabel)
			availableLabels = append(availableLabels, label)

			if newLabel == lowercaseLabel || strings.HasPrefix(newLabel, lowercaseLabel) {
				//log.Printf("[DEBUG] Found action for label '%s' in app %s (%s): %s (1)", label, app.Name, app.ID, action.Name)
				selectedAction = action

				for _, category := range categories {
					if strings.ToLower(category.Name) == strings.ToLower(app.Categories[0]) {
						selectedCategory = category
						break
					}
				}

				if newLabel == lowercaseLabel {
					exactMatch = true
					break
				}
			}
		}

		if len(selectedAction.ID) > 0 && exactMatch {
			break
		}
	}

	// Decides if we are to autocomplete the app if labels are not found
	if len(selectedAction.ID) == 0 {
		if fixLabels == true {
			//log.Printf("\n\n[DEBUG] Action not found in app %s (%s) for label '%s'. Autodiscovering and updating the app!!!\n\n", app.Name, app.ID, label)

			keys := []string{}
			for _, field := range fields {
				keys = append(keys, field.Key)
			}

			//log.Printf("[DEBUG] Calling AutofixAppLabels")

			// Make it FORCE look for a specific label if it exists, otherwise
			newApp, guessedAction := shuffle.AutofixAppLabels(app, label, keys)

			// print the found action parameters
			//for _, param := range guessedAction.Parameters {
			//	log.Printf("[DEBUG] Found parameter %s: %s", param.Name, param.Value)
			//}

			if guessedAction.Name != "" {
				//log.Printf("[DEBUG] Found action for label '%s' in app %s (%s): %s (2)", label, newApp.Name, newApp.ID, guessedAction.Name)
				selectedAction = guessedAction
			} else {
				if count > 5 {
					log.Printf("[WARNING] Too many attempts to find action for label '%s' in app %s (%s)", label, newApp.Name, newApp.ID)
				} else {
					return GetActionFromLabel(ctx, newApp, label, false, fields, count+1)
				}
			}
		}
	}

	return selectedAction, selectedCategory, availableLabels
}

func GetLocalAuth() ([]shuffle.AppAuthenticationStorage, error) {
	allAuth := []shuffle.AppAuthenticationStorage{}
	// Look for all files 
	filePath := fmt.Sprintf("%s/auth", basepath)
	files, err := os.ReadDir(filePath)
	if err != nil {
		if debug { 
			log.Printf("[ERROR] Failed reading auth directory: %s", err)
		}

		return allAuth, err
	}

	for _, file := range files {
		filename := file.Name()
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		filePath := fmt.Sprintf("%s/auth/%s", basepath, filename)
		file, err := os.Open(filePath)
		if err != nil {
			log.Printf("[ERROR] Failed opening auth file: %s", err)
			continue
		}

		defer file.Close()
		filedata, err := os.ReadFile(filePath)
		if err != nil {
			log.Printf("[ERROR] Error reading file: %s", err)
			continue
		}

		// Unmarshal the file data into the AppAuthenticationStorage struct
		newAuth := shuffle.AppAuthenticationStorage{}
		err = json.Unmarshal(filedata, &newAuth)
		if err != nil {
			log.Printf("[ERROR] Problem unmarshalling auth file %s: %s", filePath, err)
			continue
		}

		allAuth = append(allAuth, newAuth)
	}

	return allAuth, nil
}

func AuthenticateAppCli(appname string) error {
	setupEnv()

	app, err := shuffle.GetSingulApp(basepath, appname) 
	if err != nil {
		log.Printf("[ERROR] Failed getting app %s: %s", appname, err)
		return err
	}

	if len(app.Authentication.Parameters) == 0 {
		log.Printf("[DEBUG] No authentication fields found for app %s", appname)
		return errors.New("No authentication fields found")
	}

	authId := uuid.NewV4().String()
	newAuthenticationStorage := shuffle.AppAuthenticationStorage{
		Id: authId,
		App: *app,

		Label: "Authentication for " + appname,
		Active: true, 
		Created: time.Now().Unix(),
		Edited: time.Now().Unix(),

		// FIXME: This may need work.
		Encrypted: false,
	}

	if len(app.Authentication.Parameters) == 0 {
		keysFound := []string{}
		authParams := []shuffle.AuthenticationParams{}
		for _, action := range app.Actions {
			for _, param := range action.Parameters {
				if !param.Configuration {
					continue
				}

				if shuffle.ArrayContains(keysFound, param.Name) {
					continue
				}

				keysFound = append(keysFound, param.Name)
				authParams = append(authParams, shuffle.AuthenticationParams{
					Name:  param.Name,
				})
			}
		}

		if len(authParams) > 0 {
			app.Authentication.Parameters = authParams
		}
	}

	fieldnames := []string{}
	for _, param := range app.Authentication.Parameters {
		fieldnames = append(fieldnames, param.Name)
	}

	log.Printf("\n\n\n===== Authenticating %s =====\n", appname)
	log.Printf("[DEBUG] Input your fields for authentication for app %s. Fields: %s. Stored encrypted if encryption key is configured. You may always re-do this process later.\n", appname, strings.Join(fieldnames, ", "))

	keysEncrypted := false
	for _, param := range app.Authentication.Parameters {
		//if debug { 
		//	log.Printf("[DEBUG] Found parameter %s: %s", param.Name, param.Value)
		//}

		// Scanf to get input for each field
		scan := bufio.NewScanner(os.Stdin)

		paramName := param.Name
		if strings.HasSuffix(paramName, "_basic") {
			paramName = strings.Replace(paramName, "_basic", "", -1)
		}

		if len(param.Value) > 0 {
			fmt.Printf("\nEnter %s (%s): ", paramName, param.Value)
		} else {
			fmt.Printf("\nEnter %s: ", paramName)
		}

		scan.Scan()

		input := scan.Text()
		if len(input) == 0 {
			if len(param.Value) > 0 {
				input = param.Value
			} else {
				input = " "
				//if param.Required { 
				//return errors.New(fmt.Sprintf("No input provided for %s", param.Name))
				//}
			}
		}

		if strings.HasPrefix(input, "'") && strings.HasSuffix(input, "'") {
			input = strings.Trim(input, "'")
		}

		passphrase := fmt.Sprintf("%s-%s", app.ID, param.Name)
		encryptedValue, err := shuffle.HandleKeyEncryption([]byte(input), passphrase) 
		if err == nil {
			keysEncrypted = true
		} else {
			encryptedValue = []byte(input)
		}

		field := shuffle.AuthenticationStore{
			Key:  param.Name,
			Value: string(encryptedValue),
		}

		newAuthenticationStorage.Fields = append(newAuthenticationStorage.Fields, field)
	}

	newAuthenticationStorage.App.SmallImage = ""
	newAuthenticationStorage.App.LargeImage = ""
	newAuthenticationStorage.App.ChildIds = []string{}
	newAuthenticationStorage.App.Authentication = shuffle.Authentication{} 
	newAuthenticationStorage.App.Actions = []shuffle.WorkflowAppAction{}
	newAuthenticationStorage.Encrypted = keysEncrypted

	// Generate a uuid for the authentication
	// Then write it to file.json with that name 
	marshalledAuth, err := json.MarshalIndent(newAuthenticationStorage, "", "  ")
	if err != nil {
		log.Printf("[ERROR] Failed marshalling authentication storage: %s", err)
		return err
	}

	// Write the authentication storage to file
	authPath := fmt.Sprintf("%s/auth/%s.json", basepath, app.ID)
	err = os.MkdirAll(fmt.Sprintf("%s/auth", basepath), os.ModePerm)
	if err != nil {
		log.Printf("[ERROR] Failed creating auth directory: %s", err)
	}

	err = os.WriteFile(authPath, marshalledAuth, 0644)
	if err != nil {
		log.Printf("[ERROR] Failed writing authentication storage to file: %s", err)
		return err
	}

	return nil
}

// For handling the function without changing ALL the resp.X functions
type FakeResponseWriter struct {
    HeaderMap    http.Header
    Body         bytes.Buffer
    StatusCode   int
    WroteHeader  bool
}

func NewFakeResponseWriter() *FakeResponseWriter {
    return &FakeResponseWriter{
        HeaderMap:  make(http.Header),
        StatusCode: http.StatusOK, // default status
    }
}

func (f *FakeResponseWriter) Header() http.Header {
    return f.HeaderMap
}

func (f *FakeResponseWriter) Write(b []byte) (int, error) {
    if !f.WroteHeader {
        f.WriteHeader(http.StatusOK)
    }
    return f.Body.Write(b)
}

func (f *FakeResponseWriter) WriteHeader(statusCode int) {
    if f.WroteHeader {
        return // per net/http rules, WriteHeader is a no-op if already written
    }
    f.StatusCode = statusCode
    f.WroteHeader = true
}
