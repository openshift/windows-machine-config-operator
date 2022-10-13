$random=-join ((48..57) + (97..122) | Get-Random -Count 5 | % {[char]$_})
Rename-Computer -NewName winhost-$random -Force -Restart
