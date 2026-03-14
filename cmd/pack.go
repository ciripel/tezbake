package cmd

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"github.com/tez-capital/tezbake/ami"
	"github.com/tez-capital/tezbake/apps"
	"github.com/tez-capital/tezbake/apps/base"
	"github.com/tez-capital/tezbake/cli"
	"github.com/tez-capital/tezbake/constants"
	"github.com/tez-capital/tezbake/system"
	"github.com/tez-capital/tezbake/util"
	"go.alis.is/common/log"
)

const (
	PackModeFlag   = "mode"
	PackOutputFlag = "output"
	UnpackSrcFlag  = "source"

	NodeRemotePath = "node-remote-path"
	DalRemotePath  = "dal-remote-path"

	amiPackedArchiveName     = "app.zip"
	tezbakePackFormatVersion = 1
	tezbakePackMetadataFile  = "__tezbake_packed_metadata.json"
)

var remoteBundleFiles = []string{
	ami.LocatorFile,
	constants.PrivateKeyFile,
	constants.PublicKeyFile,
	ami.ElevationCredentialsFile,
	ami.ElevationCredentialsEncFile,
}

type tezbakePackedApp struct {
	Id     string `json:"id"`
	Remote bool   `json:"remote"`
}

type tezbakePackMetadata struct {
	Version int                `json:"version"`
	Mode    string             `json:"mode"`
	Apps    []tezbakePackedApp `json:"apps"`
}

type archivedRemoteFile struct {
	Data []byte
	Mode os.FileMode
}

type archivedRemoteBundle struct {
	Locator *ami.RemoteConfiguration
	Files   map[string]archivedRemoteFile
}

type remoteUnpackOverride struct {
	Remote        string
	RemoteAuth    string
	RemoteElevate ami.RemoteElevationKind
	RemotePath    string
}

func (o remoteUnpackOverride) Active() bool {
	return o.Remote != "" || o.RemoteAuth != "" || o.RemoteElevate != "" || o.RemotePath != ""
}

var packCmd = &cobra.Command{
	Use:   "pack",
	Short: "Pack installed apps into one archive",
	Long:  "Builds a merged archive containing all installed tezbake apps.",
	Run: func(cmd *cobra.Command, _ []string) {
		system.RequireElevatedUser()

		mode, err := validatePackMode(util.GetCommandStringFlagSD(cmd, PackModeFlag, "full"))
		util.AssertEE(err, "Invalid pack mode!", constants.ExitInvalidArgs)

		output := util.GetCommandStringFlagSD(cmd, PackOutputFlag, "tezbake-pack.zip")
		err = packInstalledApps(output, mode)
		util.AssertEE(err, "Failed to pack tezbake apps!", constants.ExitExternalError)
	},
}

var unpackCmd = &cobra.Command{
	Use:   "unpack",
	Short: "Unpack a merged tezbake archive",
	Long:  "Restores all tezbake apps from a merged archive created by 'tezbake pack'.",
	Run: func(cmd *cobra.Command, _ []string) {
		system.RequireElevatedUser()

		source := util.GetCommandStringFlagSD(cmd, UnpackSrcFlag, "tezbake-pack.zip")
		err := unpackInstalledApps(cmd, source)
		util.AssertEE(err, "Failed to unpack tezbake apps!", constants.ExitExternalError)
	},
}

func validatePackMode(mode string) (string, error) {
	switch mode {
	case "full", "light":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported mode %q", mode)
	}
}

func getLocalAppPath(appId string) string {
	return filepath.Join(cli.BBdir, appId)
}

func getInstalledAppsForPack() []base.BakeBuddyApp {
	result := make([]base.BakeBuddyApp, 0, len(apps.All))
	for _, app := range apps.All {
		if ami.IsAppInstalled(getLocalAppPath(app.GetId())) {
			result = append(result, app)
		}
	}
	return result
}

func packInstalledApps(output string, mode string) (resultErr error) {
	appsToPack := getInstalledAppsForPack()
	if len(appsToPack) == 0 {
		return fmt.Errorf("no installed apps found in %s", cli.BBdir)
	}

	tempDir, err := os.MkdirTemp("", "tezbake-pack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	outputFile, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	defer func() {
		if resultErr != nil {
			_ = os.Remove(output)
		}
	}()

	writer := zip.NewWriter(outputFile)
	defer func() {
		closeErr := writer.Close()
		if resultErr == nil {
			resultErr = closeErr
		}
	}()

	metadata := tezbakePackMetadata{
		Version: tezbakePackFormatVersion,
		Mode:    mode,
		Apps:    make([]tezbakePackedApp, 0, len(appsToPack)),
	}

	for _, app := range appsToPack {
		appId := app.GetId()
		localAppPath := getLocalAppPath(appId)
		appArchive, err := os.CreateTemp(tempDir, appId+"-*.zip")
		if err != nil {
			return err
		}
		appArchivePath := appArchive.Name()
		if err := appArchive.Close(); err != nil {
			return err
		}
		if err := os.Remove(appArchivePath); err != nil {
			return err
		}

		isRemote, locator := ami.IsRemoteApp(localAppPath)
		if isRemote {
			if err := packRemoteApp(locator, appArchivePath, mode); err != nil {
				return fmt.Errorf("failed to pack remote app %s: %w", appId, err)
			}
			if err := addRemoteBundleToArchive(writer, localAppPath, appId); err != nil {
				return fmt.Errorf("failed to add remote bundle for %s: %w", appId, err)
			}
		} else {
			if err := packLocalApp(localAppPath, appArchivePath, mode); err != nil {
				return fmt.Errorf("failed to pack app %s: %w", appId, err)
			}
		}

		if err := addFileToArchive(writer, path.Join("apps", appId+".zip"), appArchivePath); err != nil {
			return err
		}

		metadata.Apps = append(metadata.Apps, tezbakePackedApp{
			Id:     appId,
			Remote: isRemote,
		})
	}

	metadataRaw, err := json.MarshalIndent(metadata, "", "\t")
	if err != nil {
		return err
	}
	if err := addBytesToArchive(writer, tezbakePackMetadataFile, metadataRaw, 0644); err != nil {
		return err
	}

	log.Info("Pack successful", "output", output, "mode", mode, "apps", len(metadata.Apps))
	return nil
}

func packLocalApp(localAppPath string, output string, mode string) error {
	args := []string{"pack", "--output=" + output}
	if mode == "light" {
		args = append(args, "--light")
	}

	exitCode, err := ami.Execute(localAppPath, args...)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("ami pack exited with %d", exitCode)
	}
	return nil
}

func packRemoteApp(locator *ami.RemoteConfiguration, output string, mode string) error {
	session, err := locator.OpenAppRemoteSession()
	if err != nil {
		return err
	}
	defer session.Close()

	remoteArchivePath := path.Join("/tmp", fmt.Sprintf("tezbake-pack-%s-%d.zip", locator.App, time.Now().UnixNano()))
	defer func() {
		if err := session.RemoveRemoteFile(remoteArchivePath); err != nil {
			log.Warn("Failed to remove remote pack archive", "app", locator.App, "path", remoteArchivePath, "error", err)
		}
	}()

	args := []string{"pack", "--output=" + remoteArchivePath}
	if mode == "light" {
		args = append(args, "--light")
	}

	exitCode, err := session.ForwardAmiExecute(locator.GetRemoteAppPath(), args...)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("ami pack exited with %d", exitCode)
	}

	return session.DownloadFile(remoteArchivePath, output, 0644)
}

func addRemoteBundleToArchive(writer *zip.Writer, localAppPath string, appId string) error {
	for _, fileName := range remoteBundleFiles {
		fullPath := filepath.Join(localAppPath, fileName)
		_, err := os.Stat(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				if slices.Contains([]string{ami.LocatorFile, constants.PrivateKeyFile, constants.PublicKeyFile}, fileName) {
					return fmt.Errorf("missing required remote file %s", fullPath)
				}
				continue
			}
			return err
		}

		if err := addFileToArchive(writer, path.Join("remote", appId, fileName), fullPath); err != nil {
			return err
		}
	}
	return nil
}

func unpackInstalledApps(cmd *cobra.Command, source string) error {
	archive, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer archive.Close()

	metadata, err := loadTezbakePackMetadata(&archive.Reader)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "tezbake-unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	for _, packedApp := range metadata.Apps {
		appArchivePath := filepath.Join(tempDir, packedApp.Id+".zip")
		if err := extractArchiveFile(&archive.Reader, path.Join("apps", packedApp.Id+".zip"), appArchivePath, 0644); err != nil {
			return fmt.Errorf("failed to extract archive for %s: %w", packedApp.Id, err)
		}

		localAppPath := getLocalAppPath(packedApp.Id)
		if packedApp.Remote {
			bundle, err := loadArchivedRemoteBundle(&archive.Reader, packedApp.Id)
			if err != nil {
				return fmt.Errorf("failed to load remote locator for %s: %w", packedApp.Id, err)
			}

			override := getRemoteUnpackOverride(cmd, packedApp.Id)
			if err := unpackRemoteApp(localAppPath, packedApp.Id, appArchivePath, bundle, override); err != nil {
				return err
			}
			continue
		}

		if override := getRemoteUnpackOverride(cmd, packedApp.Id); override.Active() {
			return fmt.Errorf("remote override flags were provided for local app %s", packedApp.Id)
		}

		if err := unpackLocalApp(localAppPath, appArchivePath); err != nil {
			return err
		}
	}

	log.Info("Unpack successful", "source", source, "apps", len(metadata.Apps))
	return nil
}

func loadTezbakePackMetadata(reader *zip.Reader) (*tezbakePackMetadata, error) {
	raw, _, err := readArchiveFile(reader, tezbakePackMetadataFile)
	if err != nil {
		return nil, err
	}

	var metadata tezbakePackMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	if metadata.Version != tezbakePackFormatVersion {
		return nil, fmt.Errorf("unsupported tezbake archive version %d", metadata.Version)
	}
	if _, err := validatePackMode(metadata.Mode); err != nil {
		return nil, err
	}
	seenApps := make(map[string]struct{}, len(metadata.Apps))
	for _, app := range metadata.Apps {
		if !isSupportedAppId(app.Id) {
			return nil, fmt.Errorf("unsupported app id %q in archive", app.Id)
		}
		if _, seen := seenApps[app.Id]; seen {
			return nil, fmt.Errorf("duplicate app id %q in archive", app.Id)
		}
		seenApps[app.Id] = struct{}{}
	}
	return &metadata, nil
}

func getRemoteUnpackOverride(cmd *cobra.Command, appId string) remoteUnpackOverride {
	switch appId {
	case apps.Node.GetId():
		return remoteUnpackOverride{
			Remote:        util.GetCommandStringFlagS(cmd, NodeRemote),
			RemoteAuth:    util.GetCommandStringFlagS(cmd, NodeRemoteAuth),
			RemoteElevate: ami.RemoteElevationKind(util.GetCommandStringFlagS(cmd, NodeRemoteElevate)),
			RemotePath:    util.GetCommandStringFlagS(cmd, NodeRemotePath),
		}
	case apps.DalNode.GetId():
		return remoteUnpackOverride{
			Remote:        util.GetCommandStringFlagS(cmd, DalRemote),
			RemoteAuth:    util.GetCommandStringFlagS(cmd, DalRemoteAuth),
			RemoteElevate: ami.RemoteElevationKind(util.GetCommandStringFlagS(cmd, DalRemoteElevate)),
			RemotePath:    util.GetCommandStringFlagS(cmd, DalRemotePath),
		}
	default:
		return remoteUnpackOverride{}
	}
}

func unpackLocalApp(localAppPath string, archivePath string) error {
	if err := os.MkdirAll(localAppPath, 0755); err != nil {
		return err
	}
	if err := removeLocalRemoteArtifacts(localAppPath); err != nil {
		return err
	}

	localArchivePath := filepath.Join(localAppPath, amiPackedArchiveName)
	if err := copyFile(archivePath, localArchivePath, 0644); err != nil {
		return err
	}
	defer os.Remove(localArchivePath)

	exitCode, err := ami.Execute(localAppPath, "unpack")
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("ami unpack exited with %d", exitCode)
	}

	userName, err := getLocalAppUser(localAppPath)
	if err == nil && userName != "" {
		util.ChownR(userName, localAppPath)
	}
	return nil
}

func unpackRemoteApp(localAppPath string, appId string, archivePath string, bundle *archivedRemoteBundle, override remoteUnpackOverride) error {
	locator, err := restoreRemoteBundle(localAppPath, appId, bundle, override)
	if err != nil {
		return err
	}

	if override.Active() {
		if err := ami.PrepareRemote(localAppPath, locator, override.RemoteAuth); err != nil {
			return fmt.Errorf("failed to prepare remote for %s: %w", appId, err)
		}
	}
	if err := ami.EnsureRemoteAppDirectory(locator); err != nil {
		return fmt.Errorf("failed to prepare remote app directory for %s: %w", appId, err)
	}

	session, err := locator.OpenAppRemoteSession()
	if err != nil {
		return err
	}
	defer session.Close()

	remoteArchivePath := path.Join(locator.GetRemoteAppPath(), amiPackedArchiveName)
	defer func() {
		if err := session.RemoveRemoteFile(remoteArchivePath); err != nil {
			log.Warn("Failed to remove remote unpack archive", "app", appId, "path", remoteArchivePath, "error", err)
		}
	}()

	if err := session.UploadFile(archivePath, remoteArchivePath, 0644); err != nil {
		return err
	}

	exitCode, err := session.ForwardAmiExecute(locator.GetRemoteAppPath(), "unpack")
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("ami unpack exited with %d", exitCode)
	}

	if locator.LocalUsername != "" {
		util.ChownR(locator.LocalUsername, localAppPath)
	}
	return nil
}

func restoreRemoteBundle(localAppPath string, appId string, bundle *archivedRemoteBundle, override remoteUnpackOverride) (*ami.RemoteConfiguration, error) {
	if bundle == nil || bundle.Locator == nil {
		return nil, errors.New("missing remote locator")
	}

	if err := os.MkdirAll(localAppPath, 0755); err != nil {
		return nil, err
	}
	if err := removeLocalRemoteArtifacts(localAppPath); err != nil {
		return nil, err
	}
	if err := writeRemoteCredentialArtifacts(localAppPath, bundle); err != nil {
		return nil, err
	}

	locator := *bundle.Locator
	locator.App = appId
	locator.ElevationCredentialsDirectory = localAppPath
	locator.PrivateKey = filepath.Join(localAppPath, constants.PrivateKeyFile)
	locator.PublicKey = filepath.Join(localAppPath, constants.PublicKeyFile)

	if override.Remote != "" {
		connection := system.GetRemoteConnectionDetails(override.Remote)
		locator.Username = connection.Username
		locator.Host = connection.Host
		locator.Port = connection.Port
	}
	if override.RemoteElevate != "" {
		locator.Elevate = override.RemoteElevate
	}
	if override.RemotePath != "" {
		locator.InstancePath = override.RemotePath
	}

	if override.Active() {
		updated := ami.WriteRemoteLocator(localAppPath, &locator, true)
		return updated, nil
	}

	for _, keyFile := range []string{constants.PrivateKeyFile, constants.PublicKeyFile} {
		file, ok := bundle.Files[keyFile]
		if !ok {
			return nil, fmt.Errorf("missing %s in archive", keyFile)
		}
		if err := os.WriteFile(filepath.Join(localAppPath, keyFile), file.Data, file.Mode); err != nil {
			return nil, err
		}
	}

	if err := writeRemoteLocator(localAppPath, &locator); err != nil {
		return nil, err
	}
	return &locator, nil
}

func writeRemoteCredentialArtifacts(localAppPath string, bundle *archivedRemoteBundle) error {
	for _, fileName := range []string{ami.ElevationCredentialsFile, ami.ElevationCredentialsEncFile} {
		file, ok := bundle.Files[fileName]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(localAppPath, fileName), file.Data, file.Mode); err != nil {
			return err
		}
	}
	return nil
}

func writeRemoteLocator(localAppPath string, locator *ami.RemoteConfiguration) error {
	raw, err := json.MarshalIndent(locator, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(localAppPath, ami.LocatorFile), raw, 0644)
}

func removeLocalRemoteArtifacts(localAppPath string) error {
	for _, fileName := range remoteBundleFiles {
		err := os.Remove(filepath.Join(localAppPath, fileName))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func getLocalAppUser(localAppPath string) (string, error) {
	def, _, err := ami.FindAppDefinition(localAppPath)
	if err != nil {
		return "", err
	}
	userName, _ := def["user"].(string)
	return userName, nil
}

func loadArchivedRemoteBundle(reader *zip.Reader, appId string) (*archivedRemoteBundle, error) {
	locatorRaw, _, err := readArchiveFile(reader, path.Join("remote", appId, ami.LocatorFile))
	if err != nil {
		return nil, err
	}

	var locator ami.RemoteConfiguration
	if err := json.Unmarshal(locatorRaw, &locator); err != nil {
		return nil, err
	}

	bundle := &archivedRemoteBundle{
		Locator: &locator,
		Files:   make(map[string]archivedRemoteFile),
	}

	for _, fileName := range remoteBundleFiles {
		if fileName == ami.LocatorFile {
			continue
		}
		raw, mode, err := readArchiveFile(reader, path.Join("remote", appId, fileName))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		bundle.Files[fileName] = archivedRemoteFile{
			Data: raw,
			Mode: mode,
		}
	}
	return bundle, nil
}

func addFileToArchive(writer *zip.Writer, archivePath string, sourcePath string) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = archivePath
	header.Method = zip.Deflate

	fileWriter, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}

	fileReader, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer fileReader.Close()

	_, err = io.Copy(fileWriter, fileReader)
	return err
}

func addBytesToArchive(writer *zip.Writer, archivePath string, content []byte, mode os.FileMode) error {
	header := &zip.FileHeader{
		Name:   archivePath,
		Method: zip.Deflate,
	}
	header.SetMode(mode)

	fileWriter, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = fileWriter.Write(content)
	return err
}

func copyFile(sourcePath string, destinationPath string, mode os.FileMode) error {
	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}

	return dst.Chmod(mode)
}

func readArchiveFile(reader *zip.Reader, archivePath string) ([]byte, os.FileMode, error) {
	file := findArchiveFile(reader, archivePath)
	if file == nil {
		return nil, 0, os.ErrNotExist
	}

	fileReader, err := file.Open()
	if err != nil {
		return nil, 0, err
	}
	defer fileReader.Close()

	data, err := io.ReadAll(fileReader)
	if err != nil {
		return nil, 0, err
	}
	return data, file.Mode(), nil
}

func extractArchiveFile(reader *zip.Reader, archivePath string, destination string, mode os.FileMode) error {
	data, _, err := readArchiveFile(reader, archivePath)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, mode)
}

func findArchiveFile(reader *zip.Reader, archivePath string) *zip.File {
	for _, file := range reader.File {
		if file.Name == archivePath {
			return file
		}
	}
	return nil
}

func isSupportedAppId(appId string) bool {
	for _, app := range apps.All {
		if app.GetId() == appId {
			return true
		}
	}
	return false
}

func init() {
	packCmd.Flags().String(PackOutputFlag, "tezbake-pack.zip", "Output path for the merged archive.")
	packCmd.Flags().String(PackModeFlag, "full", "Pack mode to use for every app (full/light).")

	unpackCmd.Flags().String(UnpackSrcFlag, "tezbake-pack.zip", "Path to the merged archive.")
	unpackCmd.Flags().String(NodeRemote, "", "username@address[:port] override for a packed remote node app.")
	unpackCmd.Flags().String(NodeRemoteAuth, "", "pass|key:<path to key> override for a packed remote node app.")
	unpackCmd.Flags().String(NodeRemoteElevate, "", "Elevation override for a packed remote node app.")
	unpackCmd.Flags().String(NodeRemotePath, "", "Remote instance path override for a packed remote node app.")
	unpackCmd.Flags().String(DalRemote, "", "username@address[:port] override for a packed remote dal app.")
	unpackCmd.Flags().String(DalRemoteAuth, "", "pass|key:<path to key> override for a packed remote dal app.")
	unpackCmd.Flags().String(DalRemoteElevate, "", "Elevation override for a packed remote dal app.")
	unpackCmd.Flags().String(DalRemotePath, "", "Remote instance path override for a packed remote dal app.")

	RootCmd.AddCommand(packCmd)
	RootCmd.AddCommand(unpackCmd)
}
