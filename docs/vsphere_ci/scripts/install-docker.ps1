# Powershell script to install docker
# https://docs.microsoft.com/en-us/virtualization/windowscontainers/quick-start/set-up-environment?tabs=Windows-Server
#
# NuGet provider must be installed first to avoid the manual confirmation prompted while installing the docker package
Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force
# configure repository policy
Set-PSRepository PSGallery -InstallationPolicy Trusted
# install module with provider
Install-Module -Name DockerMsftProvider -Repository PSGallery -Force
# install docker package
Install-Package -Name docker -ProviderName DockerMsftProvider -Force
