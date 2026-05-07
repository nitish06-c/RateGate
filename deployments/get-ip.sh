#!/bin/bash
TASK_ARN=$(aws ecs list-tasks --cluster rategate --region us-east-1 --query "taskArns[0]" --output text)
echo "Task: $TASK_ARN"
ENI=$(aws ecs describe-tasks --cluster rategate --tasks $TASK_ARN --region us-east-1 --query "tasks[0].attachments[0].details[?name=='networkInterfaceId'].value" --output text)
echo "ENI: $ENI"
PUBLIC_IP=$(aws ec2 describe-network-interfaces --network-interface-ids $ENI --query "NetworkInterfaces[0].Association.PublicIp" --output text --region us-east-1)
echo "Public IP: $PUBLIC_IP"
echo "Test it: curl http://$PUBLIC_IP:8080/health"
