#!/bin/sh
rm -rf ./frpMgr

getOsName()
{
    if grep -Eqii "CentOS" /etc/issue || grep -Eq "CentOS" /etc/*-release; then
        DISTRO='CentOS'
        PM='yum'
        if command -v git >/dev/null 2>&1; then
            echo 'exists git'
            else
              echo 'no exists git'
              yum install -y git
        fi
    elif grep -Eqi "Red Hat Enterprise Linux Server" /etc/issue || grep -Eq "Red Hat Enterprise Linux Server" /etc/*-release; then
        DISTRO='RHEL'
        PM='yum'
    elif grep -Eqi "Aliyun" /etc/issue || grep -Eq "Aliyun" /etc/*-release; then
        DISTRO='Aliyun'
        PM='yum'
    elif grep -Eqi "Fedora" /etc/issue || grep -Eq "Fedora" /etc/*-release; then
        DISTRO='Fedora'
        PM='yum'
    elif grep -Eqi "Debian" /etc/issue || grep -Eq "Debian" /etc/*-release; then
        DISTRO='Debian'
        PM='apt'
        if command -v git >/dev/null 2>&1; then
          echo 'exists'
        else
          echo 'no exists'
          apt-get install -y git
        fi
    elif grep -Eqi "Ubuntu" /etc/issue || grep -Eq "Ubuntu" /etc/*-release; then
        DISTRO='Ubuntu'
        PM='apt'
        if command -v git >/dev/null 2>&1; then
            echo 'exists'
        else
            echo 'no exists'
            apt-get install -y git
        fi
    elif grep -Eqi "Raspbian" /etc/issue || grep -Eq "Raspbian" /etc/*-release; then
        DISTRO='Raspbian'
        PM='apt'
    else
        DISTRO='unknow'
    fi
    echo $DISTRO;
}
getOsName

#down file
git clone https://ghproxy.com/https://github.com/Zo3i/frpMgr.git
#enter path
cd ./frpMgr/web/src/main/docker/final
chmod -R 755 ./*
cd ./mysql
docker build -t jo/mysql .
cd ..
docker-compose down
docker-compose build
docker-compose up -d
