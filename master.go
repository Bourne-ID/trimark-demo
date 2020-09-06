// Package trimark contains an HTTP Cloud Function.
package trimark

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"image/png"
	//screenshots
	_ "image/jpeg"

	"github.com/oliamb/cutter"
	"google.golang.org/api/drive/v2"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

//FolderIDEnv name of the Drive Folder Id
const FolderIDEnv = "DRIVE_FOLDER_ID"

//UploadFolderName is the folder name for file uploads
const UploadFolderName = "UploadHere"

//ProcessedFolderName is the folder name to place processed files
const ProcessedFolderName = "Processed"

// FailedFolderName is the folder name where OCR has failed
const FailedFolderName = "Failed"

// ReportFolderName is the folder which contains the end result sheet
const ReportFolderName = "Report"

// SheetName is the file name for the report
const SheetName = "ISK Import Report"

var driveService *drive.Service
var sheetService *sheets.Service

//UploadFolderID is the ID of the folder
var UploadFolderID string

//ProcessedFolderID is the ID of the folder
var ProcessedFolderID string

//FailedFolderID is the ID of the folder
var FailedFolderID string

//ReportFolderID is the ID of the folder
var ReportFolderID string

//SheetID is the sheet ID
var SheetID string = ""

var dateRegex = `(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`
var usernameRegex = `Member Donation.*[([](?P<Member>.*)[)\]]`
var quantityZeroRegex = `(?ims)Member Donation\r\n(?P<quantity>[0-9,]*)`
var quantityFirstRegex = `(?ims)Type\r\n(?P<quantity>[0-9,]*)`
var quantitySecondRegex = `(?ims)Quantity\r\n(?P<quantity>[0-9,]*)`
var rowRegex = `.*:[A-Z](\d.*?)$`

func init() {
	var err error
	masterFolderID := os.Getenv(FolderIDEnv)

	driveService, sheetService, err = createServices("service.json")

	setupFolders(masterFolderID)

	setupSheet(ReportFolderID)

	if err != nil {
		log.Fatalf("Unable to retrieve Drive client or files: %v", err)
	}

}

// Main is the main function to do the processing
func Main(w http.ResponseWriter, r *http.Request) {
	// Step 1: Loop through the folder and find files to process
	cs, err := getFilesFromFolder(UploadFolderID, false)
	if err != nil {
		log.Fatalf("Failed to get files from folder: %v", err)
	}

	// Step 2: Process files async (waitgroups)
	var wg sync.WaitGroup

	for _, c := range cs {
		fileRef := driveService.Files.Get(c.Id)
		fileDetails, err := fileRef.Do()
		if err != nil {
			log.Fatalf("Failed to get file: %v", err)
		}

		wg.Add(1)

		go func(fileRef2 *drive.File) {
			defer wg.Done()
			mime := "application/vnd.google-apps.document"

			//Lets crop the image - remove some of the dead records
			img, err := cropImage(fileDetails)

			//And Upload this as a text file...!
			f := &drive.File{Title: fileDetails.Title + "_results", MimeType: mime}
			f.Parents = []*drive.ParentReference{&drive.ParentReference{Id: ProcessedFolderID}}

			r, err := driveService.Files.Insert(f).Media(img).Do()

			if err != nil {
				log.Fatalf("Failed to create document: %v", err)
			}

			//and now we re-read it
			textDoc, err := driveService.Files.Export(r.Id, "text/plain").Download()
			if err != nil {
				log.Fatalf("Failed to download document: %v", err)
			}
			defer textDoc.Body.Close()

			//Extract the information
			date, username, quantity, err := extractData(textDoc.Body)
			if err != nil {
				_, err2 := moveFileToFolder(fileDetails, UploadFolderID, FailedFolderID)
				if err2 != nil {
					log.Fatalf("Unable to move file to Failed: %v", err)
				}
				_, err2 = moveFileToFolder(r, UploadFolderID, FailedFolderID)
				if err2 != nil {
					log.Fatalf("Unable to move file to Failed: %v", err)
				}
			} else {
				_, err := moveFileToFolder(fileDetails, UploadFolderID, ProcessedFolderID)
				if err != nil {
					log.Fatalf("Unable to move file to Failed: %v", err)
				}
			}

			//import it into the spreadsheet
			rowID, cs, err := appendDataToSheet(date, username, quantity, r.DefaultOpenWithLink)
			if cs == "" && err != nil {
				log.Fatalf("Unable to update spreadsheet: %v", err)
			}
			if err != nil {
				log.Fatalf("Couldn't Get RowID: %v", err)
			}

			// rename the files to make it easier to scan
			renameFile(r, rowID+"-"+r.Title+"-"+cs)
			renameFile(f, rowID+"-"+r.Title+"-"+cs)

		}(fileDetails)

	}
	wg.Wait()
}

func createServices(jsonPath string) (*drive.Service, *sheets.Service, error) {
	ctx := context.Background()
	drive, err := drive.NewService(ctx, option.WithCredentialsFile(jsonPath))
	if err != nil {
		return nil, nil, err
	}

	sheet, err := sheets.NewService(ctx, option.WithCredentialsFile(jsonPath))
	if err != nil {
		return nil, nil, err
	}

	return drive, sheet, nil
}

func setupFolders(masterFolderID string) (err error) {
	folders, err := getFilesFromFolder(masterFolderID, true)
	if err != nil {
		fmt.Printf("An error occurred: %v\n", err)
	}
	check := 0
	for _, folder := range folders {
		if folder.Title == UploadFolderName {
			check = check | 1
			UploadFolderID = folder.Id
		}
		if folder.Title == ProcessedFolderName {
			check = check | 2
			ProcessedFolderID = folder.Id
		}
		if folder.Title == FailedFolderName {
			check = check | 4
			FailedFolderID = folder.Id
		}
		if folder.Title == ReportFolderName {
			check = check | 8
			ReportFolderID = folder.Id
		}
	}
	if check != 15 {
		if check&1 == 0 {
			f, err := createFolder(UploadFolderName, masterFolderID)
			if err != nil {
				return err
			}
			UploadFolderID = f.Id
		}
		if check&2 == 0 {
			f, err := createFolder(ProcessedFolderName, masterFolderID)
			if err != nil {
				return err
			}
			ProcessedFolderID = f.Id
		}
		if check&4 == 0 {
			f, err := createFolder(FailedFolderName, masterFolderID)
			if err != nil {
				return err
			}
			FailedFolderID = f.Id
		}
		if check&8 == 0 {
			f, err := createFolder(ReportFolderName, masterFolderID)
			if err != nil {
				return err
			}
			ReportFolderID = f.Id
		}
	}
	return nil
}

func setupSheet(folderID string) (err error) {
	files, err := getFilesFromFolder(folderID, false)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.Title == SheetName {
			SheetID = file.Id
			break
		}
	}

	if SheetID == "" {
		//create a new one?
		file, err := createSheet(SheetName, folderID)
		if err != nil {
			return err
		}

		ss, err := sheetService.Spreadsheets.Get(file.Id).Do()
		SheetID = ss.SpreadsheetId

		headers := []interface{}{"ID", "Import Date", "Echoes Date", "Name", "Amount", "Link"}
		values := [][]interface{}{headers}

		valueRange := &sheets.ValueRange{Values: values}

		readRange := "Sheet1!A1:F1"
		_, err = sheetService.Spreadsheets.Values.Update(SheetID, readRange, valueRange).ValueInputOption("USER_ENTERED").Do()
		if err != nil {
			return err
		}
	}
	return nil
}

func getFilesFromFolder(folderID string, foldersOnly bool) ([]*drive.File, error) {
	var cs []*drive.File
	var query = "'" + folderID + "' in parents"
	if foldersOnly {
		query = query + " AND mimeType = 'application/vnd.google-apps.folder'"
	}

	pageToken := ""
	for {
		q := driveService.Files.List()
		q = q.Q(query)
		// If we have a pageToken set, apply it to the query
		if pageToken != "" {
			q = q.PageToken(pageToken)
		}
		r, err := q.Do()
		if err != nil {
			fmt.Printf("An error occurred: %v\n", err)
			return cs, err
		}
		cs = append(cs, r.Items...)
		pageToken = r.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return cs, nil
}

func createSheet(name string, parentID string) (*drive.File, error) {
	mime := "application/vnd.google-apps.spreadsheet"
	return createEntity(name, parentID, mime)
}

func createFolder(name string, parentID string) (*drive.File, error) {
	mime := "application/vnd.google-apps.folder"
	return createEntity(name, parentID, mime)
}

func createEntity(name string, parentID string, mime string) (*drive.File, error) {
	f := &drive.File{Title: name, MimeType: mime}
	p := &drive.ParentReference{Id: parentID}
	f.Parents = []*drive.ParentReference{p}
	return driveService.Files.Insert(f).Do()
}

func extractData(textDoc io.ReadCloser) (date string, username string, quantity string, err error) {
	//Get the content of the message
	content, err := ioutil.ReadAll(textDoc)
	if err != nil {
		return "", "", "", err
	}

	//Get the date
	rDate := regexp.MustCompile(dateRegex)
	dateResults := rDate.FindStringSubmatch(string(content))
	if len(dateResults) != 2 {
		return "", "", "", errors.New("Date Not Found")
	}

	//Get the username
	rUser := regexp.MustCompile(usernameRegex)
	usernameResults := rUser.FindStringSubmatch(string(content))
	if len(usernameResults) != 2 {
		return "", "", "", errors.New("Username Not Found")
	}

	//First pass - rare occurance but important one
	rQuantity := regexp.MustCompile(quantityZeroRegex)
	quantityResults := rQuantity.FindStringSubmatch(string(content))
	if len(quantityResults) != 2 || (len(quantityResults) == 2 && quantityResults[1] == "") {
		rQuantity = regexp.MustCompile(quantityFirstRegex)
		quantityResults = rQuantity.FindStringSubmatch(string(content))
		if len(quantityResults) != 2 || (len(quantityResults) == 2 && quantityResults[1] == "") {
			//First failed, try second
			rQuantity = regexp.MustCompile(quantitySecondRegex)
			quantityResults = rQuantity.FindStringSubmatch(string(content))
			if len(quantityResults) != 2 || (len(quantityResults) == 2 && quantityResults[1] == "") {
				return "", "", "", errors.New("Quantity Not Found")
			}
		}
	}
	return dateResults[1], usernameResults[1], quantityResults[1], nil
}

func moveFileToFolder(file *drive.File, fromFolder string, toFolder string) (*drive.File, error) {
	return driveService.Files.Update(file.Id, file).RemoveParents(fromFolder).AddParents(toFolder).Do()
}

func renameFile(file *drive.File, newName string) error {
	file.Title = newName
	_, err := driveService.Files.Update(file.Id, file).Do()
	return err
}

func appendDataToSheet(date, name, amount, link string) (rowID string, checksum string, err error) {
	cs := md5.Sum([]byte(date + name + amount))
	css := hex.EncodeToString(cs[:])
	now := time.Now().Format("01-02-2006 15:04:05")
	values := [][]interface{}{[]interface{}{css, now, date, name, amount, link}}

	valueRange := &sheets.ValueRange{Values: values}

	r, err := sheetService.Spreadsheets.Values.Append(SheetID, "Sheet1!A1:G1", valueRange).InsertDataOption("INSERT_ROWS").ValueInputOption("USER_ENTERED").Do()
	if err != nil {
		return "", string(css), err
	}
	regex := regexp.MustCompile(rowRegex)
	regexResults := regex.FindStringSubmatch(r.Updates.UpdatedRange)

	if len(regexResults) != 2 {
		return "", css, errors.New("Unable to parse row which was imported")
	}
	return regexResults[1], css, nil

}

func cropImage(file *drive.File) (*bytes.Reader, error) {
	iRaw, err := driveService.Files.Get(file.Id).Download()
	if err != nil {
		log.Fatalf("Download image -> %v", err)
	}
	defer iRaw.Body.Close()

	imgByte, err := ioutil.ReadAll(iRaw.Body)
	if err != nil {
		log.Fatalf("ioutil.ReadAll -> %v", err)
	}

	img, _, err := image.Decode(bytes.NewReader(imgByte))
	if err != nil {
		log.Fatalf("image.Decode -> %v", err)
	}
	imageDetails, _, err := image.DecodeConfig(bytes.NewReader(imgByte))
	if err != nil {
		log.Fatalf("image.Decode -> %v", err)
	}
	croppedImg, err := cutter.Crop(img, cutter.Config{
		Width:  imageDetails.Width / 2,
		Height: imageDetails.Height,
	})
	if err != nil {
		log.Fatalf("cutter.Crop -> %v", err)
	}

	buf := new(bytes.Buffer)
	err = png.Encode(buf, croppedImg)
	if err != nil {
		log.Fatalf("png.Encode -> %v", err)
	}

	a := bytes.NewReader(buf.Bytes())

	return a, nil
}
