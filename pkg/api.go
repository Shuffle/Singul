package singul

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	uuid "github.com/satori/go.uuid"

	"github.com/frikky/schemaless"
	"github.com/shuffle/shuffle-shared"
)

var debug = os.Getenv("DEBUG") == "true"
var basepath = os.Getenv("SHUFFLE_FILE_LOCATION")

// Set up 3rd party connections IF necessary
var shuffleApiKey = os.Getenv("SHUFFLE_AUTHORIZATION")
var shuffleBackend = os.Getenv("SHUFFLE_BACKEND")

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
	Reason  string `json:"reason"`
}

func RunAction(ctx context.Context, value shuffle.CategoryAction) (string, error) {
	resp := http.ResponseWriter(nil)
	request := &http.Request{}
	user := shuffle.User{}

	foundResponse, err := RunActionWrapper(ctx, user, value, resp, request)

	if strings.Contains(string(foundResponse), `success": false`) {
		outputString := outputMarshal{}

		jsonerr := json.Unmarshal(foundResponse, &outputString)
		if jsonerr != nil {
			log.Printf("[WARNING] Error with unmarshaling output in run action: %s", jsonerr)
		}

		return outputString.Reason, err
	}

	return string(foundResponse), err
}

func RunActionWrapper(ctx context.Context, user shuffle.User, value shuffle.CategoryAction, resp http.ResponseWriter, request *http.Request) ([]byte, error) {
	// Ensures avoidance of nil-pointers in resp.WriteHeader()
	if resp == nil {
		resp = NewFakeResponseWriter() 
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

	log.Printf("[INFO] Running category-action '%s' in category '%s' with app %s for org %s (%s)", value.Label, value.Category, value.AppName, user.ActiveOrg.Name, user.ActiveOrg.Id)

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

	log.Printf("[INFO] Found label '%s' in category '%s'. Indexes for category: %d, and label: %d", value.Label, value.Category, foundIndex, labelIndex)

	newapps, err := shuffle.GetPrioritizedApps(ctx, user)
	if err != nil {
		log.Printf("[WARNING] Failed getting apps in category action: %s", err)
		respBody = []byte(`{"success": false, "reason": "Failed loading apps. Contact support@shuffler.io"}`)
		resp.WriteHeader(500)
		resp.Write(respBody)
		return respBody, err
	}

	org, err := shuffle.GetOrg(ctx, user.ActiveOrg.Id)
	if err != nil {
		log.Printf("[ERROR] Failed getting org %s (%s) in category action: %s", user.ActiveOrg.Name, user.ActiveOrg.Id, err)
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
				log.Printf("[DEBUG] Selected app %s (%s) with action %s", selectedApp.Name, selectedApp.ID, selectedAction.Name)

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
				log.Printf("[DEBUG] Found app - checking label: %s vs %s (%s)", app.Name, value.AppName, app.ID)
				//selectedAction, selectedCategory, availableLabels = shuffle.GetActionFromLabel(ctx, selectedApp, value.Label, true)
				selectedAction, selectedCategory, availableLabels = shuffle.GetActionFromLabel(ctx, app, value.Label, true, value.Fields, 0)
				partialMatch = false

				break

				// Finds a random match, but doesn't break in case it finds exact
			} else if selectedApp.ID == "" && len(matchName) > 0 && (strings.Contains(appName, matchName) || strings.Contains(matchName, appName)) {
				selectedApp = app

				log.Printf("[WARNING] Set selected app to PARTIAL match %s (%s) for input %s", selectedApp.Name, selectedApp.ID, value.AppName)
				selectedAction, selectedCategory, availableLabels = shuffle.GetActionFromLabel(ctx, app, value.Label, true, value.Fields, 0)

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

				tmpApp, err := shuffle.GetApp(ctx, foundApp.ObjectID, user, false)
				if err == nil {
					selectedApp = *tmpApp
					failed = false

					//log.Printf("[DEBUG] Got app %s with %d actions", selectedApp.Name, len(selectedApp.Actions))
					selectedAction, selectedCategory, availableLabels = shuffle.GetActionFromLabel(ctx, selectedApp, value.Label, true, value.Fields, 0)
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
		discoverFile := fmt.Sprintf("file_%s", fieldHash)
		file, err := shuffle.GetFile(ctx, discoverFile)

		if err != nil {
			log.Printf("[ERROR] Problem with getting file '%s' in category action autorun: %s", discoverFile, err)
		} else {
			//log.Printf("[DEBUG] Found translation file in category action: %#v. Status: %s. Category: %s", file.Id, file.Status, file.Namespace)

			if file.Status == "active" {

				fieldFileFound = true

				log.Printf("[DEBUG File found: %s", file.Filename)

				fileContent, err := shuffle.GetFileContent(ctx, file, nil)
				if err != nil {
					log.Printf("[ERROR] Failed getting file content in category action: %s", err)
					fieldFileFound = false
				}

				log.Printf("Output content: %#v", string(fileContent))
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

	/*
		if foundFields != len(baseFields) {
			log.Printf("[WARNING] Not all required fields were found in category action. Want: %#v, have: %#v", baseFields, value.Fields)

			resp.WriteHeader(400)
			resp.Write([]byte(fmt.Sprintf(`{"success": false, "reason": "Not all required fields are set. This can be autocompleted from fields you fille in", "label": "%s", "missing_fields": "%s"}`, value.Label, strings.Join(missingFields, ","))))
			return

		}
	*/
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

	auth := []shuffle.AppAuthenticationStorage{}
	foundAuthenticationId := value.AuthenticationId
	if len(foundAuthenticationId) == 0 {
		// 1. Get auth
		// 2. Append the auth
		// 3. Run!

		auth, err = shuffle.GetAllWorkflowAppAuth(ctx, user.ActiveOrg.Id)
		if err != nil {
			log.Printf("[WARNING] Failed getting auths for org %s: %s", user.ActiveOrg.Id, err)
		} else {
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
			auth, err = shuffle.GetAllWorkflowAppAuth(ctx, user.ActiveOrg.Id)
			if err != nil {
				log.Printf("[DEBUG] Failed loading auth: %s", err)
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

	if !value.SkipWorkflow {
		log.Printf("\n\n[INFO] Adding workflow %s\n\n", parentWorkflow.ID)
	}

	/*
		refUrl := ""
		if project.Environment == "cloud" {
			location := "europe-west2"
			if len(os.Getenv("SHUFFLE_GCEPROJECT_REGION")) > 0 {
				location = os.Getenv("SHUFFLE_GCEPROJECT_REGION")
			}

			gceProject := os.Getenv("SHUFFLE_GCEPROJECT")
			functionName := fmt.Sprintf("%s-%s", selectedApp.Name, selectedApp.ID)
			functionName = strings.ToLower(strings.Replace(strings.Replace(strings.Replace(strings.Replace(strings.Replace(functionName, "_", "-", -1), ":", "-", -1), "-", "-", -1), " ", "-", -1), ".", "-", -1))

			refUrl = fmt.Sprintf("https://%s-%s.cloudfunctions.net/%s", location, gceProject, functionName)
		}
	*/

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
	selectedAction = shuffle.GetOrgspecificParameters(ctx, *org, selectedAction)

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

		for _, field := range value.Fields {
			if !shuffle.ArrayContains(handledRequiredFields, field.Key) {
				missingFields = append(missingFields, field.Key)
			}
		}

		log.Printf("[WARNING] Not all required fields were handled. Missing: %#v. Should force use of all fields? Handled fields: %3v", missingFields, handledRequiredFields)
	}

	// Send request to /api/v1/conversation with this data
	baseUrl := fmt.Sprintf("https://shuffler.io")
	if len(os.Getenv("SHUFFLE_CLOUDRUN_URL")) > 0 {
		baseUrl = fmt.Sprintf("%s", os.Getenv("SHUFFLE_CLOUDRUN_URL"))
	}

	client := shuffle.GetExternalClient(baseUrl)

	selectedAction.AppName = selectedApp.Name
	selectedAction.AppID = selectedApp.ID
	selectedAction.AppVersion = selectedApp.AppVersion

	for _, missing := range missingFields {
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

		log.Printf("[DEBUG] GOT BODY: %#v", fixedBody)
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
		log.Printf("[DEBUG] Found file content map (Reverse Schemaless): %#v", fieldFileContentMap)

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

			log.Printf("[DEBUG] Found value for key %#v: '%s'", key, mapValue)

			// Check if the key exists in the parameters
			for paramIndex, param := range selectedAction.Parameters {
				log.Printf("[DEBUG] Checking param %s with value %+v", param.Name, mappedFieldSplit)

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
					log.Printf("\n\n\n[DEBUG] Found body field for file content: %s. Location: %#v, Value: %#v\n\n\n", key, strings.Join(mappedFieldSplit, "."), mapValue)

					newBody := param.Value

					mapToSearch := map[string]interface{}{}
					err := json.Unmarshal([]byte(newBody), &mapToSearch)
					if err != nil {
						log.Printf("[WARNING] Failed unmarshalling body for file content: %s. Body: %s", err, string(newBody))
						continue
					}

					// Finds where in the body the value should be placed
					location := strings.Join(mappedFieldSplit[1:], ".")
					outputMap := schemaless.MapValueToLocation(mapToSearch, location, mapValue.(string))

					// Marshal back to JSON
					marshalledMap, err := json.Marshal(outputMap)
					if err != nil {
						log.Printf("[WARNING] Failed marshalling body for file content: %s", err)
					} else {
						log.Printf("[DEBUG] setting parameter value to %s", string(marshalledMap))
						selectedAction.Parameters[paramIndex].Value = string(marshalledMap)
						log.Printf("[DEBUG] Found value for key %s: %s -- %+v", key, mapValue, missingFields)
						missingFields = shuffle.RemoveFromArray(missingFields, key)
						log.Printf("[DEBUG] Found value for key %s: %s -- %+v", key, mapValue, missingFields)
					}
				} else {
					log.Printf("\n\n\n[DEBUG] Found map with actionParameter %s with value %s\n\n\n", param.Name, mapValue)

					// YOLO
					selectedAction.Parameters[paramIndex].Value = mapValue.(string)

					log.Printf("[DEBUG] Found value for key %s: %s -- %+v", key, mapValue, missingFields)
					missingFields = shuffle.RemoveFromArray(missingFields, key)
					missingFields = shuffle.RemoveFromArray(missingFields, selectedAction.Parameters[paramIndex].Name)
					log.Printf("[DEBUG] Found value for key %s: %s -- %+v", key, mapValue, missingFields)

					secondAction.Parameters = selectedAction.Parameters
				}

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
		log.Printf("\n\n\n[DEBUG] Missing fields for action: %#v\n\n\n", missingFields)

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

		//formattedQuery := fmt.Sprintf("Use any of the fields '%s' with app %s to '%s'.", strings.Join(formattedQueryFields, "&"), strings.ReplaceAll(selectedApp.Name, "_", " "), strings.ReplaceAll(value.Label, "_", " "))
		formattedQuery := fmt.Sprintf("Use the fields '%s' with app %s to '%s'.", strings.Join(formattedQueryFields, "&"), strings.ReplaceAll(selectedApp.Name, "_", " "), strings.ReplaceAll(value.Label, "_", " "))

		newQueryInput := shuffle.QueryInput{
			Query:        formattedQuery,
			OutputFormat: "action", // To run the action (?)
			//OutputFormat: "action_parameters", 	// To get the action parameters back so we can run it manually

			//Label: value.Label,
			Category: value.Category,

			AppId:      selectedApp.ID,
			AppName:    selectedApp.Name,
			ActionName: selectedAction.Name,
			Parameters: selectedAction.Parameters,
		}

		// JSON marshal and send it back in to /api/conversation with type "action"
		marshalledBody, err := json.Marshal(newQueryInput)
		if err != nil {
			log.Printf("[WARNING] Failed marshalling action: %s", err)
			respBody = []byte(`{"success": false, "reason": "Failed marshalling action"}`)
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		//streamUrl = "http://localhost:5002"
		conversationUrl := fmt.Sprintf("%s/api/v1/conversation", baseUrl)
		log.Printf("[DEBUG][AI] Sending single conversation execution to %s", conversationUrl)

		// Check if "execution_id" & "authorization" queries exist
		if len(request.Header.Get("Authorization")) == 0 && len(request.URL.Query().Get("execution_id")) > 0 && len(request.URL.Query().Get("authorization")) > 0 {
			conversationUrl = fmt.Sprintf("%s?execution_id=%s&authorization=%s", conversationUrl, request.URL.Query().Get("execution_id"), request.URL.Query().Get("authorization"))
		}

		req, err := http.NewRequest(
			"POST",
			conversationUrl,
			bytes.NewBuffer(marshalledBody),
		)

		if err != nil {
			log.Printf("[WARNING] Error in new request for execute generated workflow: %s", err)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed preparing new request. Contact support."}`))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		log.Printf("[DEBUG] MISSINGFIELDS: %#v", missingFields)
		log.Printf("[DEBUG] LOCAL AI REQUEST SENT TO %s", conversationUrl)

		req.Header.Add("Authorization", request.Header.Get("Authorization"))
		req.Header.Add("Org-Id", request.Header.Get("Org-Id"))

		authorization = request.Header.Get("Authorization")
		orgId = request.Header.Get("Org-Id")

		newresp, err := client.Do(req)
		if err != nil {
			log.Printf("[WARNING] Error running body for execute generated workflow: %s", err)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed running app %s (%s). Contact support."}`, selectedAction.Name, selectedAction.AppID))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

		defer newresp.Body.Close()
		responseBody, err := ioutil.ReadAll(newresp.Body)
		if err != nil {
			log.Printf("[WARNING] Failed reading body for execute generated workflow: %s", err)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed unmarshalling app response. Contact support."}`))
			resp.WriteHeader(500)
			resp.Write(respBody)
			return respBody, err
		}

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

		log.Printf("[DEBUG] Taking params and image from second action and adding to workflow")

		// FIXME: Image?
		//secondAction.Parameters = newSecondAction.Parameters
		//secondAction.LargeImage = newSecondAction.LargeImage
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

	if value.SkipWorkflow {
		//log.Printf("[DEBUG] Skipping workflow generation, and instead attempting to directly run the action. This is only applicable IF the action is atomic (skip_workflow=true).")
		if len(missingFields) > 0 {
			log.Printf("[WARNING] Not all required fields were found in category action. Want: %#v in action %s", missingFields, selectedAction.Name)
			respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Not all required fields are set", "label": "%s", "missing_fields": "%s", "action": "%s", "api_debugger_url": "%s"}`, value.Label, strings.Join(missingFields, ","), selectedAction.Name, fmt.Sprintf("https://shuffler.io/apis/%s", selectedApp.ID)))
			resp.WriteHeader(400)
			resp.Write(respBody)
			return respBody, errors.New("Not all required fields were found")
		}

		// FIXME: Make a check for IF we have filled in all fields or not
		for paramIndex, param := range secondAction.Parameters {
			//if param.Configuration {
			//	continue
			//}
			//log.Printf("[DEBUG] Param: %s, Value: %s", param.Name, param.Value)
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

		// FIXME: Delete disabled for now (April 2nd 2024)
		// This is due to needing debug capabilities
		if len(request.Header.Get("Authorization")) > 0 {
			tmpAuth := request.Header.Get("Authorization")

			if strings.HasPrefix(tmpAuth, "Bearer") {
				tmpAuth = strings.Replace(tmpAuth, "Bearer ", "", 1)
			}

			authorization = tmpAuth
		}

		// The app run url to use. Default delete is false
		shouldDelete := "false"
		apprunUrl := fmt.Sprintf("%s/api/v1/apps/%s/run?delete=%s", baseUrl, secondAction.AppID, shouldDelete)

		if len(request.Header.Get("Authorization")) == 0 && len(request.URL.Query().Get("execution_id")) > 0 && len(request.URL.Query().Get("authorization")) > 0 {
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
		maxAttempts := 7

		log.Printf("[DEBUG][AI] Sending single API run execution to %s", apprunUrl)
		for i := 0; i < maxAttempts; i++ {

			// Sends back how many translations happened
			// -url is just for the app to parse it :(
			attemptString := "x-translation-attempt-url"
			if _, ok := resp.Header()[attemptString]; ok {
				resp.Header().Set(attemptString, fmt.Sprintf("%d", i+1))
			} else {
				resp.Header().Add(attemptString, fmt.Sprintf("%d", i+1))
			}

			//log.Printf("[DEBUG] Attempt preparedAction: %s", string(preparedAction))

			// The request that goes to the CORRECT app
			req, err := http.NewRequest(
				"POST",
				apprunUrl,
				bytes.NewBuffer(preparedAction),
			)

			if err != nil {
				log.Printf("[WARNING] Error in new request for execute generated app run: %s", err)
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
				log.Printf("[WARNING] Error running body for execute generated app run: %s", err)
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
			apprunBody, err := ioutil.ReadAll(newresp.Body)
			if err != nil {
				log.Printf("[WARNING] Failed reading body for execute generated app run: %s", err)
				respBody = []byte(fmt.Sprintf(`{"success": false, "reason": "Failed unmarshalling app response. Contact support."}`))
				resp.WriteHeader(500)
				resp.Write(respBody)
				return respBody, err
			}

			// Parse success struct
			successStruct := shuffle.ResultChecker{}
			json.Unmarshal(apprunBody, &successStruct)

			httpOutput, marshalledBody, httpParseErr := shuffle.FindHttpBody(apprunBody)
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
				log.Printf("[DEBUG] Found status from schemaless: %d. Should save the current fields as new base", httpOutput.Status)

				parsedParameterMap := map[string]interface{}{}
				for _, param := range secondAction.Parameters {
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

					// FIXME: Skipping anything but body for now
					if param.Name != "body" {
						continue
					}

					go shuffle.UploadParameterBase(context.Background(), user.ActiveOrg.Id, selectedApp.ID, secondAction.Name, param.Name, param.Value)
					//err = uploadParameterBase(ctx, user.ActiveOrg.Id, selectedApp.ID, secondAction.Name, param.Name, param.Value)
					//if err != nil {
					//	log.Printf("[WARNING] Failed uploading parameter base for %s: %s", param.Name, err)
					//}
				}

				if len(fieldHash) > 0 && fieldFileFound == false {
					inputFieldMap := map[string]interface{}{}
					for _, field := range value.Fields {
						inputFieldMap[field.Key] = field.Value
					}

					// Finds location of some data in another part of the data. This is to have a predefined location in subsequent requests
					// Allows us to map text -> field and not just field -> text (2-way)

					go func() {
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

							log.Printf("Reversed fields (%d): %s", finishedFields, reversed)
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

								returnedId, err := shuffle.UploadFile(context.Background(), file, encryptionKey, []byte(reversed))
								if err != nil {
									log.Printf("[ERROR] Problem uploading file: %s", err)
								} else {
									log.Printf("[DEBUG] Uploaded file with ID: %s", returnedId)
								}
							}
						}
					}()
				}
			} else {
				// Parses out data from the output
				// Reruns the app with the new parameters
				//log.Printf("HTTP PARSE ERR: %#v", httpParseErr)
				if strings.Contains(strings.ToLower(fmt.Sprintf("%s", httpParseErr)), "status: ") {
					log.Printf("\n\n\n[DEBUG] Found status code in error: %s\n\n\n", err)

					outputString, outputAction, err, additionalInfo := shuffle.FindNextApiStep(secondAction, apprunBody, additionalInfo, inputQuery, originalAppname)
					log.Printf("[DEBUG]\n==== AUTOCORRECT ====\nOUTPUTSTRING: %s\nADDITIONALINFO: %s", outputString, additionalInfo)

					// Rewriting before continuing
					// This makes the NEXT loop iterator run with the
					// output params from this one
					if err == nil && len(outputAction.Parameters) > 0 {
						log.Printf("[DEBUG] Found output action: %s", outputAction.Name)
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

						log.Printf("\n\nRUNNING WITH NEW PARAMS. Index: %d\n\n", i)
						continue

					} else {
						log.Printf("[ERROR] Problem in autocorrect (%d):\n%#v\nParams: %d", i, err, len(outputAction.Parameters))
						if i < maxAttempts-1 {
							continue
						}
					}
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

			authConfig := fmt.Sprintf("%s,%s,%s,%s", baseUrl, authorization, orgId, optionalExecutionId)

			//log.Printf("\n\n[DEBUG] BASEURL FOR SCHEMALESS: %s\nAUTHCONFIG: %s\n\n", streamUrl, authConfig)

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
					parsedTranslation.RawResponse = nil
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

		log.Printf("\n\n\n[DEBUG] Done in autocorrect loop\n\n\n")
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

	err = shuffle.SetWorkflow(ctx, parentWorkflow, parentWorkflow.ID)
	if err != nil {
		log.Printf("[WARNING] Failed setting workflow during category run: %s", err)
	}

	/*
		workflowBytes, err := json.Marshal(parentWorkflow)
		if err != nil {
			log.Printf("[WARNING] Failed marshal of workflow during cache setting: %s", err)
			resp.WriteHeader(400)
			resp.Write([]byte(fmt.Sprintf(`{"success": false, "reason": "Failed packing workflow. Please try again and contact support if this persists."}`)))
			return
		}

		// Force it to ONLY be in cache? Means it times out.
		err = SetCache(ctx, fmt.Sprintf("workflow_%s", parentWorkflow.ID), workflowBytes, 10)
		if err != nil {
			log.Printf("[WARNING] Failed setting cache for workflow during category run: %s", err)

			shuffle.SetWorkflow(ctx, parentWorkflow, parentWorkflow.ID)
		}
	*/

	log.Printf("[DEBUG] Done preparing workflow '%s' (%s) to be ran for category action %s", parentWorkflow.Name, parentWorkflow.ID, selectedAction.Name)

	/*














	 */

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
	/*
		baseUrl := fmt.Sprintf("https://shuffler.io")
		if len(os.Getenv("SHUFFLE_GCEPROJECT")) > 0 && len(os.Getenv("SHUFFLE_GCEPROJECT_LOCATION")) > 0 {
			baseUrl = fmt.Sprintf("https://%s.%s.r.appspot.com", os.Getenv("SHUFFLE_GCEPROJECT"), os.Getenv("SHUFFLE_GCEPROJECT_LOCATION"))
		}

		if len(os.Getenv("SHUFFLE_CLOUDRUN_URL")) > 0 {
			baseUrl = fmt.Sprintf("%s", os.Getenv("SHUFFLE_CLOUDRUN_URL"))
		}
	*/

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
