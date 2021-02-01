Set-PSRepository PSGallery -InstallationPolicy Trusted
Install-Module -Name DockerMsftProvider -Repository PSGallery -Force
Install-Package -Name docker -ProviderName DockerMsftProvider -Force
